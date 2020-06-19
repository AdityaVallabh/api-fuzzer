# OpenAPI Testing Meqanized

Meqa generates and runs test suites using your OpenAPI (formerly Swagger) spec in YAML. It makes REST API testing easy by generating useful test patterns - no coding needed.

## Demo

![gif](https://i.imgur.com/prWsMEi.gif)

## Highlights

* Understands the object relationships and generates tests that use the right objects and values.
* Verifies the REST call results against known objects and values.
* Verifies the REST call results against OpenAPI schema.
* Verifies the REST call results against request as well as previous responses.
* Produces easy to understand and easy to modify intermediate files for customization.
* Performs positive/negative/datatype fuzzing and logs failures

## Getting Started

First, build the binaries.

* `make binary`: Builds and places `mqgen` and `mqgo` binaries in `bin/` directory

Use your OpenAPI spec (e.g., petstore.yml) to generate the test plan files.
The commands are:

* `bin/mqgen -d testdata -s testdata/petstore_meqa.yml -a path`: Given the test directory path and OpenAPI spec file, `mqgen` generates a test plan `path.yml` in `testdata`.
* `bin/mqgo run -d testdata -s testdata/petstore_meqa.yml -p testdata/path.yml`: The tests in `path.yml` are executed and results are logged to `results.yml`.

The run step takes a generated test plan file (path.yml in the above example).

* simple.yml just exercises a few simple APIs to expose obvious issues, such as lack of api keys.
* path.yml exercises CRUD patterns grouped by the REST path.
* The test yaml files can be edited to add in your own test suites. We allow overriding global, test suite and test parameters, as well as chaining output to input parameters. See [meqa format](docs/format.md) for more details.

## Usage

### mqgen

```
$ mqgen --help
Usage of mqgen:
  -a string
    	the algorithm - simple, object, path, all (default "all")
  -d string
    	the directory where we put the generated files (default "meqa_data")
  -m string
    	the paths in this file will be ignored
  -s string
    	the swagger.yml file location (default "meqa_data/swagger.yml")
  -v	turn on verbose mode
  -w string
    	the allowed APIs file location
```

### mqgo 
```
$ mqgo run --help
Usage of run:
  -a string
    	the api token for bearer HTTP authentication
  -b int
    	batch size (default 10)
  -d string
    	the directory where meqa config, log and output files reside (default "meqa_data")
  -f string
    	fuzz type: none, positive, datatype or negative (default "none")
  -h string
    	the host's base url
  -l string
    	the dataset path
  -p string
    	the test plan file name
  -r string
    	the test result file name (default result.yml in meqa_data dir)
  -re
    	reproduce failures
  -s string
    	the meqa generated OpenAPI (Swagger) spec file path
  -t string
    	the test to run (default "all")
  -u string
    	the username for basic HTTP authentication
  -v	turn on verbose mode
  -w string
    	the password for basic HTTP authentication
```

## Docs

For details see the [docs](docs) directory.
