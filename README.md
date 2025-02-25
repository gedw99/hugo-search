# hugo-search [![Build Status](https://github.com/tischda/hugo-search/actions/workflows/test.yml/badge.svg)](https://github.com/tischda/hugo-search/actions/workflows/test.yml)

A [Bleve](http://www.blevesearch.com) search server for your [Hugo](http://gohugo.io) site.

### Dependencies

* Hugo
* Bleve search


### Compile

Outside of GOPATH:

~~~
git clone https://github.com/tischda/hugo-search
cd hugo-search
make build
~~~


### Usage

~~~
Usage of hugo-search:
  -addr string
        http listen address (default ":8080")
  -hugoPath string
        path of the hugo site (default ".")
  -indexPath string
        path of the bleve index (default "indexes/search.bleve")
  -verbose    verbose output
  -version
        print version and exit
~~~

### Query index

~~~
$ curl http://localhost:8080/api/search.bleve/_search -d '{"query":{"query":"lorem"}}'
{"request":{"query":{"query":"lorem","boost":1},"size":0,"from":0,"highlight":null,"fields":null,"facets":null,"explain":false},"hits":[],"total_hits":2,"max_score":0.15713484143442302,"took":0,"facets":{}}

{"status":{"total":1,"failed":0,"successful":1},"request":{"query":{"query":"lorem","boost":1},"size":0,"from":0,"highlight":null,"fields":null,"facets":null,"explain":false},"hits":[],"total_hits":3,"max_score":0.15713484143442302,"took":0,"facets":{}}
~~~

### Explore index with bleve-explorer

Warning: Cannot use while `hugo-search` is running.

~~~
go get github.com/blevesearch/bleve-explorer

bleve-explorer -dataDir indexes
~~~

check on [http://localhost:8095/](http://localhost:8095/)
