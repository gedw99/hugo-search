package toml

import (
	"encoding"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2/internal/ast"
	"github.com/pelletier/go-toml/v2/internal/tracker"
)

// Unmarshal deserializes a TOML document into a Go value.
//
// It is a shortcut for Decoder.Decode() with the default options.
func Unmarshal(data []byte, v interface{}) error {
	p := parser{}
	p.Reset(data)
	d := decoder{p: &p}

	return d.FromParser(v)
}

// Decoder reads and decode a TOML document from an input stream.
type Decoder struct {
	// input
	r io.Reader

	// global settings
	strict bool
}

// NewDecoder creates a new Decoder that will read from r.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: r}
}

// SetStrict toggles decoding in stict mode.
//
// When the decoder is in strict mode, it will record fields from the document
// that could not be set on the target value. In that case, the decoder returns
// a StrictMissingError that can be used to retrieve the individual errors as
// well as generate a human readable description of the missing fields.
func (d *Decoder) SetStrict(strict bool) {
	d.strict = strict
}

// Decode the whole content of r into v.
//
// By default, values in the document that don't exist in the target Go value
// are ignored. See Decoder.SetStrict() to change this behavior.
//
// When a TOML local date, time, or date-time is decoded into a time.Time, its
// value is represented in time.Local timezone. Otherwise the approriate Local*
// structure is used.
//
// Empty tables decoded in an interface{} create an empty initialized
// map[string]interface{}.
//
// Types implementing the encoding.TextUnmarshaler interface are decoded from a
// TOML string.
//
// When decoding a number, go-toml will return an error if the number is out of
// bounds for the target type (which includes negative numbers when decoding
// into an unsigned int).
//
// Type mapping
//
// List of supported TOML types and their associated accepted Go types:
//
//   String           -> string
//   Integer          -> uint*, int*, depending on size
//   Float            -> float*, depending on size
//   Boolean          -> bool
//   Offset Date-Time -> time.Time
//   Local Date-time  -> LocalDateTime, time.Time
//   Local Date       -> LocalDate, time.Time
//   Local Time       -> LocalTime, time.Time
//   Array            -> slice and array, depending on elements types
//   Table            -> map and struct
//   Inline Table     -> same as Table
//   Array of Tables  -> same as Array and Table
func (d *Decoder) Decode(v interface{}) error {
	b, err := ioutil.ReadAll(d.r)
	if err != nil {
		return fmt.Errorf("toml: %w", err)
	}

	p := parser{}
	p.Reset(b)
	dec := decoder{
		p: &p,
		strict: strict{
			Enabled: d.strict,
		},
	}

	return dec.FromParser(v)
}

type decoder struct {
	// Which parser instance in use for this decoding session.
	p *parser

	// Flag indicating that the current expression is stashed.
	// If set to true, calling nextExpr will not actually pull a new expression
	// but turn off the flag instead.
	stashedExpr bool

	// Skip expressions until a table is found. This is set to true when a
	// table could not be create (missing field in map), so all KV expressions
	// need to be skipped.
	skipUntilTable bool

	// Tracks position in Go arrays.
	// This is used when decoding [[array tables]] into Go arrays. Given array
	// tables are separate TOML expression, we need to keep track of where we
	// are at in the Go array, as we can't just introspect its size.
	arrayIndexes map[reflect.Value]int

	// Tracks keys that have been seen, with which type.
	seen tracker.SeenTracker

	// Strict mode
	strict strict
}

func (d *decoder) expr() *ast.Node {
	return d.p.Expression()
}

func (d *decoder) nextExpr() bool {
	if d.stashedExpr {
		d.stashedExpr = false
		return true
	}
	return d.p.NextExpression()
}

func (d *decoder) stashExpr() {
	d.stashedExpr = true
}

func (d *decoder) arrayIndex(shouldAppend bool, v reflect.Value) int {
	if d.arrayIndexes == nil {
		d.arrayIndexes = make(map[reflect.Value]int, 1)
	}

	idx, ok := d.arrayIndexes[v]

	if !ok {
		d.arrayIndexes[v] = 0
	} else if shouldAppend {
		idx++
		d.arrayIndexes[v] = idx
	}

	return idx
}

func (d *decoder) FromParser(v interface{}) error {
	r := reflect.ValueOf(v)
	if r.Kind() != reflect.Ptr {
		return fmt.Errorf("toml: decoding can only be performed into a pointer, not %s", r.Kind())
	}

	if r.IsNil() {
		return fmt.Errorf("toml: decoding pointer target cannot be nil")
	}

	r = r.Elem()
	if r.Kind() == reflect.Interface && r.IsNil() {
		newMap := map[string]interface{}{}
		r.Set(reflect.ValueOf(newMap))
	}

	err := d.fromParser(r)
	if err == nil {
		return d.strict.Error(d.p.data)
	}

	var e *decodeError
	if errors.As(err, &e) {
		return wrapDecodeError(d.p.data, e)
	}

	return err
}

func (d *decoder) fromParser(root reflect.Value) error {
	for d.nextExpr() {
		err := d.handleRootExpression(d.expr(), root)
		if err != nil {
			return err
		}
	}

	return d.p.Error()
}

/*
Rules for the unmarshal code:

- The stack is used to keep track of which values need to be set where.
- handle* functions <=> switch on a given ast.Kind.
- unmarshalX* functions need to unmarshal a node of kind X.
- An "object" is either a struct or a map.
*/

func (d *decoder) handleRootExpression(expr *ast.Node, v reflect.Value) error {
	var x reflect.Value
	var err error

	if !(d.skipUntilTable && expr.Kind == ast.KeyValue) {
		err = d.seen.CheckExpression(expr)
		if err != nil {
			return err
		}
	}

	switch expr.Kind {
	case ast.KeyValue:
		if d.skipUntilTable {
			return nil
		}
		x, err = d.handleKeyValue(expr, v)
	case ast.Table:
		d.skipUntilTable = false
		d.strict.EnterTable(expr)
		x, err = d.handleTable(expr.Key(), v)
	case ast.ArrayTable:
		d.skipUntilTable = false
		d.strict.EnterArrayTable(expr)
		x, err = d.handleArrayTable(expr.Key(), v)
	default:
		panic(fmt.Errorf("parser should not permit expression of kind %s at document root", expr.Kind))
	}

	if d.skipUntilTable {
		if expr.Kind == ast.Table || expr.Kind == ast.ArrayTable {
			d.strict.MissingTable(expr)
		}
	} else if err == nil && x.IsValid() {
		v.Set(x)
	}

	return err
}

func (d *decoder) handleArrayTable(key ast.Iterator, v reflect.Value) (reflect.Value, error) {
	if key.Next() {
		return d.handleArrayTablePart(key, v)
	}
	return d.handleKeyValues(v)
}

func (d *decoder) handleArrayTableCollectionLast(key ast.Iterator, v reflect.Value) (reflect.Value, error) {
	switch v.Kind() {
	case reflect.Interface:
		elem := v.Elem()
		if !elem.IsValid() {
			elem = reflect.New(sliceInterfaceType).Elem()
			elem.Set(reflect.MakeSlice(sliceInterfaceType, 0, 16))
		} else if elem.Kind() == reflect.Slice {
			if elem.Type() != sliceInterfaceType {
				elem = reflect.New(sliceInterfaceType).Elem()
				elem.Set(reflect.MakeSlice(sliceInterfaceType, 0, 16))
			} else if !elem.CanSet() {
				nelem := reflect.New(sliceInterfaceType).Elem()
				nelem.Set(reflect.MakeSlice(sliceInterfaceType, elem.Len(), elem.Cap()))
				reflect.Copy(nelem, elem)
				elem = nelem
			}
		}
		return d.handleArrayTableCollectionLast(key, elem)
	case reflect.Ptr:
		elem := v.Elem()
		if !elem.IsValid() {
			ptr := reflect.New(v.Type().Elem())
			v.Set(ptr)
			elem = ptr.Elem()
		}

		elem, err := d.handleArrayTableCollectionLast(key, elem)
		if err != nil {
			return reflect.Value{}, err
		}
		v.Elem().Set(elem)

		return v, nil
	case reflect.Slice:
		elemType := v.Type().Elem()
		if elemType.Kind() == reflect.Interface {
			elemType = mapStringInterfaceType
		}
		elem := reflect.New(elemType).Elem()
		elem2, err := d.handleArrayTable(key, elem)
		if err != nil {
			return reflect.Value{}, err
		}
		if elem2.IsValid() {
			elem = elem2
		}
		return reflect.Append(v, elem), nil
	case reflect.Array:
		idx := d.arrayIndex(true, v)
		if idx >= v.Len() {
			return v, fmt.Errorf("toml: cannot decode array table into %s at position %d", v.Type(), idx)
		}
		elem := v.Index(idx)
		_, err := d.handleArrayTable(key, elem)
		return v, err
	}

	return d.handleArrayTable(key, v)
}

// When parsing an array table expression, each part of the key needs to be
// evaluated like a normal key, but if it returns a collection, it also needs to
// point to the last element of the collection. Unless it is the last part of
// the key, then it needs to create a new element at the end.
func (d *decoder) handleArrayTableCollection(key ast.Iterator, v reflect.Value) (reflect.Value, error) {
	if key.IsLast() {
		return d.handleArrayTableCollectionLast(key, v)
	}

	switch v.Kind() {
	case reflect.Ptr:
		elem := v.Elem()
		if !elem.IsValid() {
			ptr := reflect.New(v.Type().Elem())
			v.Set(ptr)
			elem = ptr.Elem()
		}

		elem, err := d.handleArrayTableCollection(key, elem)
		if err != nil {
			return reflect.Value{}, err
		}
		v.Elem().Set(elem)

		return v, nil
	case reflect.Slice:
		elem := v.Index(v.Len() - 1)
		x, err := d.handleArrayTable(key, elem)
		if err != nil || d.skipUntilTable {
			return reflect.Value{}, err
		}
		if x.IsValid() {
			elem.Set(x)
		}

		return v, err
	case reflect.Array:
		idx := d.arrayIndex(false, v)
		if idx >= v.Len() {
			return v, fmt.Errorf("toml: cannot decode array table into %s at position %d", v.Type(), idx)
		}
		elem := v.Index(idx)
		_, err := d.handleArrayTable(key, elem)
		return v, err
	}

	return d.handleArrayTable(key, v)
}

func (d *decoder) handleKeyPart(key ast.Iterator, v reflect.Value, nextFn handlerFn, makeFn valueMakerFn) (reflect.Value, error) {
	var rv reflect.Value

	// First, dispatch over v to make sure it is a valid object.
	// There is no guarantee over what it could be.
	switch v.Kind() {
	case reflect.Ptr:
		elem := v.Elem()
		if !elem.IsValid() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		elem = v.Elem()
		return d.handleKeyPart(key, elem, nextFn, makeFn)
	case reflect.Map:
		// Create the key for the map element. For now assume it's a string.
		mk := reflect.ValueOf(string(key.Node().Data))

		// If the map does not exist, create it.
		if v.IsNil() {
			v = reflect.MakeMap(v.Type())
			rv = v
		}

		mv := v.MapIndex(mk)
		set := false
		if !mv.IsValid() {
			// If there is no value in the map, create a new one according to
			// the map type. If the element type is interface, create either a
			// map[string]interface{} or a []interface{} depending on whether
			// this is the last part of the array table key.

			t := v.Type().Elem()
			if t.Kind() == reflect.Interface {
				mv = makeFn()
			} else {
				mv = reflect.New(t).Elem()
			}
			set = true
		} else if mv.Kind() == reflect.Interface {
			mv = mv.Elem()
			if !mv.IsValid() {
				mv = makeFn()
			}
			set = true
		} else if !mv.CanAddr() {
			t := v.Type().Elem()
			oldmv := mv
			mv = reflect.New(t).Elem()
			mv.Set(oldmv)
			set = true
		}

		x, err := nextFn(key, mv)
		if err != nil {
			return reflect.Value{}, err
		}

		if x.IsValid() {
			mv = x
			set = true
		}

		if set {
			v.SetMapIndex(mk, mv)
		}
	case reflect.Struct:
		f, found := structField(v, string(key.Node().Data))
		if !found {
			d.skipUntilTable = true
			return reflect.Value{}, nil
		}

		x, err := nextFn(key, f)
		if err != nil || d.skipUntilTable {
			return reflect.Value{}, err
		}
		if x.IsValid() {
			f.Set(x)
		}
	case reflect.Interface:
		if v.Elem().IsValid() {
			v = v.Elem()
		} else {
			v = reflect.MakeMap(mapStringInterfaceType)
		}

		x, err := d.handleKeyPart(key, v, nextFn, makeFn)
		if err != nil {
			return reflect.Value{}, err
		}
		if x.IsValid() {
			v = x
		}
		rv = v
	default:
		panic(fmt.Errorf("unhandled part: %s", v.Kind()))
	}

	return rv, nil
}

// HandleArrayTablePart navigates the Go structure v using the key v. It is
// only used for the prefix (non-last) parts of an array-table. When
// encountering a collection, it should go to the last element.
func (d *decoder) handleArrayTablePart(key ast.Iterator, v reflect.Value) (reflect.Value, error) {
	var makeFn valueMakerFn
	if key.IsLast() {
		makeFn = makeSliceInterface
	} else {
		makeFn = makeMapStringInterface
	}
	return d.handleKeyPart(key, v, d.handleArrayTableCollection, makeFn)
}

// HandleTable returns a reference when it has checked the next expression but
// cannot handle it.
func (d *decoder) handleTable(key ast.Iterator, v reflect.Value) (reflect.Value, error) {
	if v.Kind() == reflect.Slice {
		if v.Len() == 0 {
			return reflect.Value{}, newDecodeError(key.Node().Data, "cannot store a table in a slice")
		}
		elem := v.Index(v.Len() - 1)
		x, err := d.handleTable(key, elem)
		if err != nil {
			return reflect.Value{}, err
		}
		if x.IsValid() {
			elem.Set(x)
		}
		return reflect.Value{}, nil
	}
	if key.Next() {
		// Still scoping the key
		return d.handleTablePart(key, v)
	}
	// Done scoping the key.
	// Now handle all the key-value expressions in this table.
	return d.handleKeyValues(v)
}

// Handle root expressions until the end of the document or the next
// non-key-value.
func (d *decoder) handleKeyValues(v reflect.Value) (reflect.Value, error) {
	var rv reflect.Value
	for d.nextExpr() {
		expr := d.expr()
		if expr.Kind != ast.KeyValue {
			// Stash the expression so that fromParser can just loop and use
			// the right handler.
			// We could just recurse ourselves here, but at least this gives a
			// chance to pop the stack a bit.
			d.stashExpr()
			break
		}

		err := d.seen.CheckExpression(expr)
		if err != nil {
			return reflect.Value{}, err
		}

		x, err := d.handleKeyValue(expr, v)
		if err != nil {
			return reflect.Value{}, err
		}
		if x.IsValid() {
			v = x
			rv = x
		}
	}
	return rv, nil
}

type (
	handlerFn    func(key ast.Iterator, v reflect.Value) (reflect.Value, error)
	valueMakerFn func() reflect.Value
)

func makeMapStringInterface() reflect.Value {
	return reflect.MakeMap(mapStringInterfaceType)
}

func makeSliceInterface() reflect.Value {
	return reflect.MakeSlice(sliceInterfaceType, 0, 16)
}

func (d *decoder) handleTablePart(key ast.Iterator, v reflect.Value) (reflect.Value, error) {
	return d.handleKeyPart(key, v, d.handleTable, makeMapStringInterface)
}

func (d *decoder) tryTextUnmarshaler(node *ast.Node, v reflect.Value) (bool, error) {
	// Special case for time, because we allow to unmarshal to it from
	// different kind of AST nodes.
	if v.Type() == timeType {
		return false, nil
	}

	if v.CanAddr() && v.Addr().Type().Implements(textUnmarshalerType) {
		err := v.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText(node.Data)
		if err != nil {
			return false, newDecodeError(d.p.Raw(node.Raw), "error calling UnmarshalText: %w", err)
		}

		return true, nil
	}

	return false, nil
}

func (d *decoder) handleValue(value *ast.Node, v reflect.Value) error {
	for v.Kind() == reflect.Ptr {
		v = initAndDereferencePointer(v)
	}

	ok, err := d.tryTextUnmarshaler(value, v)
	if ok || err != nil {
		return err
	}

	switch value.Kind {
	case ast.String:
		return d.unmarshalString(value, v)
	case ast.Integer:
		return d.unmarshalInteger(value, v)
	case ast.Float:
		return d.unmarshalFloat(value, v)
	case ast.Bool:
		return d.unmarshalBool(value, v)
	case ast.DateTime:
		return d.unmarshalDateTime(value, v)
	case ast.LocalDate:
		return d.unmarshalLocalDate(value, v)
	case ast.LocalTime:
		return d.unmarshalLocalTime(value, v)
	case ast.LocalDateTime:
		return d.unmarshalLocalDateTime(value, v)
	case ast.InlineTable:
		return d.unmarshalInlineTable(value, v)
	case ast.Array:
		return d.unmarshalArray(value, v)
	default:
		panic(fmt.Errorf("handleValue not implemented for %s", value.Kind))
	}
}

func (d *decoder) unmarshalArray(array *ast.Node, v reflect.Value) error {
	switch v.Kind() {
	case reflect.Slice:
		if v.IsNil() {
			v.Set(reflect.MakeSlice(v.Type(), 0, 16))
		} else {
			v.SetLen(0)
		}
	case reflect.Array:
		// arrays are always initialized
	case reflect.Interface:
		elem := v.Elem()
		if !elem.IsValid() {
			elem = reflect.New(sliceInterfaceType).Elem()
			elem.Set(reflect.MakeSlice(sliceInterfaceType, 0, 16))
		} else if elem.Kind() == reflect.Slice {
			if elem.Type() != sliceInterfaceType {
				elem = reflect.New(sliceInterfaceType).Elem()
				elem.Set(reflect.MakeSlice(sliceInterfaceType, 0, 16))
			} else if !elem.CanSet() {
				nelem := reflect.New(sliceInterfaceType).Elem()
				nelem.Set(reflect.MakeSlice(sliceInterfaceType, elem.Len(), elem.Cap()))
				reflect.Copy(nelem, elem)
				elem = nelem
			}
		}
		err := d.unmarshalArray(array, elem)
		if err != nil {
			return err
		}
		v.Set(elem)
		return nil
	default:
		// TODO: use newDecodeError, but first the parser needs to fill
		//   array.Data.
		return fmt.Errorf("toml: cannot store array in Go type %s", v.Kind())
	}

	elemType := v.Type().Elem()

	it := array.Children()
	idx := 0
	for it.Next() {
		n := it.Node()

		// TODO: optimize
		if v.Kind() == reflect.Slice {
			elem := reflect.New(elemType).Elem()

			err := d.handleValue(n, elem)
			if err != nil {
				return err
			}

			v.Set(reflect.Append(v, elem))
		} else { // array
			if idx >= v.Len() {
				return nil
			}
			elem := v.Index(idx)
			err := d.handleValue(n, elem)
			if err != nil {
				return err
			}
			idx++
		}
	}

	return nil
}

func (d *decoder) unmarshalInlineTable(itable *ast.Node, v reflect.Value) error {
	// Make sure v is an initialized object.
	switch v.Kind() {
	case reflect.Map:
		if v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}
	case reflect.Struct:
	// structs are always initialized.
	case reflect.Interface:
		elem := v.Elem()
		if !elem.IsValid() {
			elem = reflect.MakeMap(mapStringInterfaceType)
			v.Set(elem)
		}
		return d.unmarshalInlineTable(itable, elem)
	default:
		return newDecodeError(itable.Data, "cannot store inline table in Go type %s", v.Kind())
	}

	it := itable.Children()
	for it.Next() {
		n := it.Node()

		x, err := d.handleKeyValue(n, v)
		if err != nil {
			return err
		}
		if x.IsValid() {
			v = x
		}
	}

	return nil
}

func (d *decoder) unmarshalDateTime(value *ast.Node, v reflect.Value) error {
	dt, err := parseDateTime(value.Data)
	if err != nil {
		return err
	}

	v.Set(reflect.ValueOf(dt))
	return nil
}

func (d *decoder) unmarshalLocalDate(value *ast.Node, v reflect.Value) error {
	ld, err := parseLocalDate(value.Data)
	if err != nil {
		return err
	}

	if v.Type() == timeType {
		cast := ld.AsTime(time.Local)
		v.Set(reflect.ValueOf(cast))
		return nil
	}

	v.Set(reflect.ValueOf(ld))

	return nil
}

func (d *decoder) unmarshalLocalTime(value *ast.Node, v reflect.Value) error {
	lt, rest, err := parseLocalTime(value.Data)
	if err != nil {
		return err
	}

	if len(rest) > 0 {
		return newDecodeError(rest, "extra characters at the end of a local time")
	}

	v.Set(reflect.ValueOf(lt))
	return nil
}

func (d *decoder) unmarshalLocalDateTime(value *ast.Node, v reflect.Value) error {
	ldt, rest, err := parseLocalDateTime(value.Data)
	if err != nil {
		return err
	}

	if len(rest) > 0 {
		return newDecodeError(rest, "extra characters at the end of a local date time")
	}

	if v.Type() == timeType {
		cast := ldt.AsTime(time.Local)

		v.Set(reflect.ValueOf(cast))
		return nil
	}

	v.Set(reflect.ValueOf(ldt))

	return nil
}

func (d *decoder) unmarshalBool(value *ast.Node, v reflect.Value) error {
	b := value.Data[0] == 't'

	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(b)
	case reflect.Interface:
		v.Set(reflect.ValueOf(b))
	default:
		return newDecodeError(value.Data, "cannot assign boolean to a %t", b)
	}

	return nil
}

func (d *decoder) unmarshalFloat(value *ast.Node, v reflect.Value) error {
	f, err := parseFloat(value.Data)
	if err != nil {
		return err
	}

	switch v.Kind() {
	case reflect.Float64:
		v.SetFloat(f)
	case reflect.Float32:
		if f > math.MaxFloat32 {
			return newDecodeError(value.Data, "number %f does not fit in a float32", f)
		}
		v.SetFloat(f)
	case reflect.Interface:
		v.Set(reflect.ValueOf(f))
	default:
		return newDecodeError(value.Data, "float cannot be assigned to %s", v.Kind())
	}

	return nil
}

func (d *decoder) unmarshalInteger(value *ast.Node, v reflect.Value) error {
	const (
		maxInt = int64(^uint(0) >> 1)
		minInt = -maxInt - 1
	)

	i, err := parseInteger(value.Data)
	if err != nil {
		return err
	}

	var r reflect.Value

	switch v.Kind() {
	case reflect.Int64:
		v.SetInt(i)
		return nil
	case reflect.Int32:
		if i < math.MinInt32 || i > math.MaxInt32 {
			return fmt.Errorf("toml: number %d does not fit in an int32", i)
		}

		r = reflect.ValueOf(int32(i))
	case reflect.Int16:
		if i < math.MinInt16 || i > math.MaxInt16 {
			return fmt.Errorf("toml: number %d does not fit in an int16", i)
		}

		r = reflect.ValueOf(int16(i))
	case reflect.Int8:
		if i < math.MinInt8 || i > math.MaxInt8 {
			return fmt.Errorf("toml: number %d does not fit in an int8", i)
		}

		r = reflect.ValueOf(int8(i))
	case reflect.Int:
		if i < minInt || i > maxInt {
			return fmt.Errorf("toml: number %d does not fit in an int", i)
		}

		r = reflect.ValueOf(int(i))
	case reflect.Uint64:
		if i < 0 {
			return fmt.Errorf("toml: negative number %d does not fit in an uint64", i)
		}

		r = reflect.ValueOf(uint64(i))
	case reflect.Uint32:
		if i < 0 || i > math.MaxUint32 {
			return fmt.Errorf("toml: negative number %d does not fit in an uint32", i)
		}

		r = reflect.ValueOf(uint32(i))
	case reflect.Uint16:
		if i < 0 || i > math.MaxUint16 {
			return fmt.Errorf("toml: negative number %d does not fit in an uint16", i)
		}

		r = reflect.ValueOf(uint16(i))
	case reflect.Uint8:
		if i < 0 || i > math.MaxUint8 {
			return fmt.Errorf("toml: negative number %d does not fit in an uint8", i)
		}

		r = reflect.ValueOf(uint8(i))
	case reflect.Uint:
		if i < 0 {
			return fmt.Errorf("toml: negative number %d does not fit in an uint", i)
		}

		r = reflect.ValueOf(uint(i))
	case reflect.Interface:
		r = reflect.ValueOf(i)
	default:
		return fmt.Errorf("toml: cannot store TOML integer into a Go %s", v.Kind())
	}

	if !r.Type().AssignableTo(v.Type()) {
		r = r.Convert(v.Type())
	}

	v.Set(r)

	return nil
}

func (d *decoder) unmarshalString(value *ast.Node, v reflect.Value) error {
	switch v.Kind() {
	case reflect.String:
		v.SetString(string(value.Data))
	case reflect.Interface:
		v.Set(reflect.ValueOf(string(value.Data)))
	default:
		return newDecodeError(d.p.Raw(value.Raw), "cannot store TOML string into a Go %s", v.Kind())
	}

	return nil
}

func (d *decoder) handleKeyValue(expr *ast.Node, v reflect.Value) (reflect.Value, error) {
	d.strict.EnterKeyValue(expr)

	v, err := d.handleKeyValueInner(expr.Key(), expr.Value(), v)
	if d.skipUntilTable {
		d.strict.MissingField(expr)
		d.skipUntilTable = false
	}

	d.strict.ExitKeyValue(expr)

	return v, err
}

func (d *decoder) handleKeyValueInner(key ast.Iterator, value *ast.Node, v reflect.Value) (reflect.Value, error) {
	if key.Next() {
		// Still scoping the key
		return d.handleKeyValuePart(key, value, v)
	}
	// Done scoping the key.
	// v is whatever Go value we need to fill.
	return reflect.Value{}, d.handleValue(value, v)
}

func (d *decoder) handleKeyValuePart(key ast.Iterator, value *ast.Node, v reflect.Value) (reflect.Value, error) {
	// contains the replacement for v
	var rv reflect.Value

	// First, dispatch over v to make sure it is a valid object.
	// There is no guarantee over what it could be.
	switch v.Kind() {
	case reflect.Map:
		mk := reflect.ValueOf(string(key.Node().Data))

		keyType := v.Type().Key()
		if !mk.Type().AssignableTo(keyType) {
			if !mk.Type().ConvertibleTo(keyType) {
				return reflect.Value{}, fmt.Errorf("toml: cannot convert map key of type %s to expected type %s", mk.Type(), keyType)
			}

			mk = mk.Convert(keyType)
		}

		// If the map does not exist, create it.
		if v.IsNil() {
			v = reflect.MakeMap(v.Type())
			rv = v
		}

		mv := v.MapIndex(mk)
		set := false
		if !mv.IsValid() {
			set = true
			mv = reflect.New(v.Type().Elem()).Elem()
		} else {
			if key.IsLast() {
				var x interface{}
				mv = reflect.ValueOf(&x).Elem()
				set = true
			}
		}

		nv, err := d.handleKeyValueInner(key, value, mv)
		if err != nil {
			return reflect.Value{}, err
		}
		if nv.IsValid() {
			mv = nv
			set = true
		}

		if set {
			v.SetMapIndex(mk, mv)
		}
	case reflect.Struct:
		f, found := structField(v, string(key.Node().Data))
		if !found {
			d.skipUntilTable = true
			break
		}

		x, err := d.handleKeyValueInner(key, value, f)
		if err != nil {
			return reflect.Value{}, err
		}

		if x.IsValid() {
			f.Set(x)
		}
	case reflect.Interface:
		v = v.Elem()

		// Following encoding/toml: decoding an object into an interface{}, it
		// needs to always hold a map[string]interface{}. This is for the types
		// to be consistent whether a previous value was set or not.
		if !v.IsValid() || v.Type() != mapStringInterfaceType {
			v = reflect.MakeMap(mapStringInterfaceType)
		}

		x, err := d.handleKeyValuePart(key, value, v)
		if err != nil {
			return reflect.Value{}, err
		}
		if x.IsValid() {
			v = x
		}
		rv = v
	case reflect.Ptr:
		elem := v.Elem()
		if !elem.IsValid() {
			ptr := reflect.New(v.Type().Elem())
			v.Set(ptr)
			rv = v
			elem = ptr.Elem()
		}

		elem2, err := d.handleKeyValuePart(key, value, elem)
		if err != nil {
			return reflect.Value{}, err
		}
		if elem2.IsValid() {
			elem = elem2
		}
		v.Elem().Set(elem)
	default:
		return reflect.Value{}, fmt.Errorf("unhandled kv part: %s", v.Kind())
	}

	return rv, nil
}

func initAndDereferencePointer(v reflect.Value) reflect.Value {
	var elem reflect.Value
	if v.IsNil() {
		ptr := reflect.New(v.Type().Elem())
		v.Set(ptr)
	}
	elem = v.Elem()
	return elem
}

type fieldPathsMap = map[string][]int

type fieldPathsCache struct {
	m map[reflect.Type]fieldPathsMap
	l sync.RWMutex
}

func (c *fieldPathsCache) get(t reflect.Type) (fieldPathsMap, bool) {
	c.l.RLock()
	paths, ok := c.m[t]
	c.l.RUnlock()

	return paths, ok
}

func (c *fieldPathsCache) set(t reflect.Type, m fieldPathsMap) {
	c.l.Lock()
	c.m[t] = m
	c.l.Unlock()
}

var globalFieldPathsCache = fieldPathsCache{
	m: map[reflect.Type]fieldPathsMap{},
	l: sync.RWMutex{},
}

func structField(v reflect.Value, name string) (reflect.Value, bool) {
	//nolint:godox
	// TODO: cache this, and reduce allocations
	fieldPaths, ok := globalFieldPathsCache.get(v.Type())
	if !ok {
		fieldPaths = map[string][]int{}

		path := make([]int, 0, 16)

		var walk func(reflect.Value)
		walk = func(v reflect.Value) {
			t := v.Type()
			for i := 0; i < t.NumField(); i++ {
				l := len(path)
				path = append(path, i)
				f := t.Field(i)

				if f.Anonymous {
					walk(v.Field(i))
				} else if f.PkgPath == "" {
					// only consider exported fields
					fieldName, ok := f.Tag.Lookup("toml")
					if !ok {
						fieldName = f.Name
					}

					pathCopy := make([]int, len(path))
					copy(pathCopy, path)

					fieldPaths[fieldName] = pathCopy
					// extra copy for the case-insensitive match
					fieldPaths[strings.ToLower(fieldName)] = pathCopy
				}
				path = path[:l]
			}
		}

		walk(v)

		globalFieldPathsCache.set(v.Type(), fieldPaths)
	}

	path, ok := fieldPaths[name]
	if !ok {
		path, ok = fieldPaths[strings.ToLower(name)]
	}

	if !ok {
		return reflect.Value{}, false
	}

	return v.FieldByIndex(path), true
}
