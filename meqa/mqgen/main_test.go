package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AdityaVallabh/swagger_meqa/meqa/mqutil"
)

func TestMqgen(t *testing.T) {
	mqutil.Logger = mqutil.NewStdLogger()
	wd, _ := os.Getwd()
	meqaPath := filepath.Join(wd, "../../testdata")
	swaggerPath := filepath.Join(meqaPath, "petstore_meqa.yml")
	algorithm := "all"
	verbose := false
	whitelistPath := ""
	ignoredPathsPath := ""
	run(&meqaPath, &swaggerPath, &algorithm, &verbose, &whitelistPath, &ignoredPathsPath)
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
