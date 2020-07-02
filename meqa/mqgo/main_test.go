package main

import (
	"os"
	"testing"

	"github.com/AdityaVallabh/swagger_meqa/meqa/mqutil"
)

func TestInterfaceEqual(t *testing.T) {
	criteria := []interface{}{
		[]interface{}{"str1", "2", 3, []interface{}{4, "5"}},
		[]interface{}{"str1", "2", 3, []interface{}{4, "5"}},
		map[string]interface{}{"k1": "v1", "k2": 2.00, "k3": []interface{}{}, "k4": map[string]interface{}{"nestedMap": "worksToo"}},
		map[string]interface{}{"k1": "v1", "k2": 2.00, "k3": []interface{}{}, "k4": map[string]interface{}{"nestedMap": "7"}},
	}
	actual := []interface{}{
		[]interface{}{"str1", "2", "3", []interface{}{4, "5"}, "extra"},
		[]interface{}{"str1", "2", 3, []interface{}{"5"}}, // missing 4 in nested slice
		map[string]interface{}{"k1": "v1", "k2": "2", "k3": []interface{}{"emptySliceIsASubset"}, "k4": map[string]interface{}{"nestedMap": "worksToo", "extra": 0}},
		map[string]interface{}{"k1": "v1", "k2": "2", "k3": []interface{}{}, "k4": map[string]interface{}{"nestedMap": 7, "extra": 0}}, // int -> str is ok but not str -> int
	}
	expected := []bool{
		true,
		false,
		true,
		false,
	}
	for i := 0; i < len(expected); i++ {
		if mqutil.InterfaceEquals(criteria[i], actual[i]) != expected[i] {
			os.Exit(1)
		}
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
