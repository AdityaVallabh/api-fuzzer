package main

import (
	"flag"
	"fmt"

	"os"
	"path/filepath"

	"github.com/AdityaVallabh/swagger_meqa/meqa/mqutil"

	"github.com/AdityaVallabh/swagger_meqa/meqa/mqswag"

	"github.com/AdityaVallabh/swagger_meqa/meqa/mqplan"
)

const (
	meqaDataDir = "meqa_data"
	algoSimple  = "simple"
	algoObject  = "object"
	algoPath    = "path"
	algoAll     = "all"
)

var algoList []string = []string{algoSimple, algoObject, algoPath}

func main() {
	mqutil.Logger = mqutil.NewStdLogger()

	swaggerJSONFile := filepath.Join(meqaDataDir, "swagger.yml")
	meqaPath := flag.String("d", meqaDataDir, "the directory where we put the generated files")
	swaggerFile := flag.String("s", swaggerJSONFile, "the swagger.yml file location")
	algorithm := flag.String("a", "all", "the algorithm - simple, object, path, all")
	verbose := flag.Bool("v", false, "turn on verbose mode")
	whitelistedAPIsFile := flag.String("w", "", "name of the file (that lists out all whitelisted APIs) along with its relative path. Example testdata/whitelist.cfg")
	ignoredPathsFile := flag.String("i", "", "name of the file (that lists out all ignored paths in APIs) along with its relative path. Example testdata/ignorePaths.cfg")

	flag.Parse()
	run(meqaPath, swaggerFile, algorithm, verbose, whitelistedAPIsFile, ignoredPathsFile)
}

func run(meqaPath *string, swaggerFile *string, algorithm *string, verbose *bool, whitelistedAPIsFile *string, ignoredPathsFile *string) {
	mqutil.Verbose = *verbose

	swaggerJsonPath := *swaggerFile
	if fi, err := os.Stat(swaggerJsonPath); os.IsNotExist(err) || fi.Mode().IsDir() {
		fmt.Printf("Can't load swagger file at the following location %s", swaggerJsonPath)
		os.Exit(1)
	}
	whitelistedAPIs := GetList(*whitelistedAPIsFile)
	ignoredPaths := GetList(*ignoredPathsFile)
	testPlanPath := *meqaPath
	if fi, err := os.Stat(testPlanPath); os.IsNotExist(err) {
		err = os.Mkdir(testPlanPath, 0755)
		if err != nil {
			fmt.Printf("Can't create the directory at %s\n", testPlanPath)
			os.Exit(1)
		}
	} else if !fi.Mode().IsDir() {
		fmt.Printf("The specified location is not a directory: %s\n", testPlanPath)
		os.Exit(1)
	}

	// loading swagger.json
	swagger, err := mqswag.CreateSwaggerFromURL(swaggerJsonPath, *meqaPath)
	if err != nil {
		mqutil.Logger.Printf("Error: %s", err.Error())
		os.Exit(1)
	}
	dag := mqswag.NewDAG()
	err = swagger.AddToDAG(dag)
	if err != nil {
		mqutil.Logger.Printf("Error: %s", err.Error())
		os.Exit(1)
	}

	dag.Sort()
	dag.CheckWeight()

	var plansToGenerate []string
	if *algorithm == algoAll {
		plansToGenerate = algoList
	} else {
		plansToGenerate = append(plansToGenerate, *algorithm)
	}

	for _, algo := range plansToGenerate {
		var testPlan *mqplan.TestPlan
		switch algo {
		case algoPath:
			testPlan, err = mqplan.GeneratePathTestPlan(swagger, dag, whitelistedAPIs, ignoredPaths)
		case algoObject:
			testPlan, err = mqplan.GenerateTestPlan(swagger, dag)
		default:
			testPlan, err = mqplan.GenerateSimpleTestPlan(swagger, dag)
		}
		if err != nil {
			mqutil.Logger.Printf("Error: %s", err.Error())
			os.Exit(1)
		}
		testPlanFile := filepath.Join(testPlanPath, algo+".yml")
		err = testPlan.DumpToFile(testPlanFile)
		if err != nil {
			mqutil.Logger.Printf("Error: %s", err.Error())
			os.Exit(1)
		}
		fmt.Println("Test plans generated at:", testPlanFile)
	}
}

func GetList(path string) map[string]bool {
	if len(path) > 0 {
		list, err := mqswag.GetListFromFile(path)
		if err != nil {
			fmt.Println("Can't read file at the following location:", path)
			os.Exit(1)
		}
		return list
	}
	return nil
}
