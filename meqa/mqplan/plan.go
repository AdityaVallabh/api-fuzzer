package mqplan

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/resty.v1"
	"gopkg.in/yaml.v2"

	"github.com/AdityaVallabh/swagger_meqa/meqa/mqutil"

	"github.com/AdityaVallabh/swagger_meqa/meqa/mqswag"
)

const (
	MeqaInit  = "meqa_init"
	MeqaFails = ".mqfails.jsonl"
	MetaFile  = "meta.yml"
)

type TestParams struct {
	QueryParams  map[string]interface{} `yaml:"queryParams,omitempty"`
	FormParams   map[string]interface{} `yaml:"formParams,omitempty"`
	PathParams   map[string]interface{} `yaml:"pathParams,omitempty"`
	HeaderParams map[string]interface{} `yaml:"headerParams,omitempty"`
	BodyParams   interface{}            `yaml:"bodyParams,omitempty"`
}

// Copy the parameters from src. If there is a conflict dst will be overwritten.
func (dst *TestParams) Copy(src *TestParams) {
	dst.QueryParams = mqutil.MapCombine(dst.QueryParams, src.QueryParams)
	dst.FormParams = mqutil.MapCombine(dst.FormParams, src.FormParams)
	dst.PathParams = mqutil.MapCombine(dst.PathParams, src.PathParams)
	dst.HeaderParams = mqutil.MapCombine(dst.HeaderParams, src.HeaderParams)

	if caseMap, caseIsMap := dst.BodyParams.(map[string]interface{}); caseIsMap {
		if testMap, testIsMap := src.BodyParams.(map[string]interface{}); testIsMap {
			dst.BodyParams = mqutil.MapCombine(caseMap, testMap)
			// for map, just combine and return
			return
		}
	}
	dst.BodyParams = src.BodyParams
}

// Add the parameters from src. If there is a conflict the dst original value will be kept.
func (dst *TestParams) Add(src *TestParams) {
	dst.QueryParams = mqutil.MapAdd(dst.QueryParams, src.QueryParams)
	dst.FormParams = mqutil.MapAdd(dst.FormParams, src.FormParams)
	dst.PathParams = mqutil.MapAdd(dst.PathParams, src.PathParams)
	dst.HeaderParams = mqutil.MapAdd(dst.HeaderParams, src.HeaderParams)

	if caseMap, caseIsMap := dst.BodyParams.(map[string]interface{}); caseIsMap {
		if testMap, testIsMap := src.BodyParams.(map[string]interface{}); testIsMap {
			dst.BodyParams = mqutil.MapAdd(caseMap, testMap)
			// for map, just combine and return
			return
		}
	}

	if dst.BodyParams == nil {
		dst.BodyParams = src.BodyParams
	}
}

type TestSuite struct {
	Tests []*Test
	Name  string

	// test suite parameters
	TestParams `yaml:",inline,omitempty" json:",inline,omitempty"`
	Strict     bool

	// Authentication
	Username string
	Password string
	ApiToken string

	plan *TestPlan
	db   *mqswag.DB // objects generated/obtained as part of this suite

	comment string
}

func CreateTestSuite(name string, tests []*Test, plan *TestPlan) *TestSuite {
	c := TestSuite{}
	c.Name = name
	c.Tests = tests
	(&c.TestParams).Copy(&plan.TestParams)
	c.Strict = plan.Strict

	c.Username = plan.Username
	c.Password = plan.Password
	c.ApiToken = plan.ApiToken

	c.plan = plan
	return &c
}

// Represents all the test suites in the DSL.
type TestPlan struct {
	SuiteMap  map[string](*TestSuite)
	SuiteList [](*TestSuite)
	db        *mqswag.DB
	swagger   *mqswag.Swagger

	// global parameters
	TestParams `yaml:",inline,omitempty" json:",inline,omitempty"`
	Strict     bool
	BaseURL    string

	// Authentication
	Username string
	Password string
	ApiToken string

	// Run result.
	resultList   []*Test
	ResultCounts map[string]int

	OldFailuresMap map[string]map[string]map[string]map[interface{}]bool // endpoint->method->field->value
	NewFailures    []*mqswag.Payload

	comment  string
	FuzzType int
	Repro    bool
}

// Add a new TestSuite, returns whether the Case is successfully added.
func (plan *TestPlan) Add(testSuite *TestSuite) error {
	if _, exist := plan.SuiteMap[testSuite.Name]; exist {
		str := fmt.Sprintf("Duplicate name %s found in test plan", testSuite.Name)
		mqutil.Logger.Println(str)
		return errors.New(str)
	}
	plan.SuiteMap[testSuite.Name] = testSuite
	plan.SuiteList = append(plan.SuiteList, testSuite)
	return nil
}

func (plan *TestPlan) AddFromString(data string) error {
	var suiteMap map[string]([]*Test)
	err := yaml.Unmarshal([]byte(data), &suiteMap)
	if err != nil {
		mqutil.Logger.Printf("The following is not a valud TestSuite:\n%s", data)
		return err
	}

	for suiteName, testList := range suiteMap {
		if suiteName == MeqaInit {
			// global parameters
			for _, t := range testList {
				t.Init(nil)
				(&plan.TestParams).Copy(&t.TestParams)
				plan.Strict = t.Strict
			}

			continue
		}
		testSuite := CreateTestSuite(suiteName, testList, plan)
		for _, t := range testList {
			t.Init(testSuite)
		}
		err = plan.Add(testSuite)
		if err != nil {
			return err
		}
	}
	return nil
}

func (plan *TestPlan) InitFromFile(path string, db *mqswag.DB) error {
	plan.Init(db.Swagger, db)

	data, err := ioutil.ReadFile(path)
	if err != nil {
		mqutil.Logger.Printf("Can't open the following file: %s", path)
		mqutil.Logger.Println(err.Error())
		return err
	}
	chunks := strings.Split(string(data), "---")
	for _, chunk := range chunks {
		plan.AddFromString(chunk)
	}
	return nil
}

func (plan *TestPlan) ReadFails(path string) error {
	f, err := os.Open(filepath.Join(path, MeqaFails))
	defer f.Close()
	if err != nil {
		return err
	}
	failures := make(map[string]map[string]map[string]map[interface{}]bool)
	d := json.NewDecoder(f)
	for {
		var v mqswag.Payload
		if err := d.Decode(&v); err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		if failures[v.Endpoint] == nil {
			failures[v.Endpoint] = make(map[string]map[string]map[interface{}]bool)
		}
		if failures[v.Endpoint][v.Method] == nil {
			failures[v.Endpoint][v.Method] = make(map[string]map[interface{}]bool)
		}
		if failures[v.Endpoint][v.Method][v.Field] == nil {
			failures[v.Endpoint][v.Method][v.Field] = make(map[interface{}]bool)
		}
		failures[v.Endpoint][v.Method][v.Field][v.Value] = true
	}
	plan.OldFailuresMap = failures
	return nil
}

func WriteComment(comment string, f *os.File) {
	ar := strings.Split(comment, "\n")
	for _, line := range ar {
		f.WriteString("# " + line + "\n")
	}
}

func (plan *TestPlan) DumpToFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if len(plan.comment) > 0 {
		WriteComment(plan.comment, f)
	}
	for _, testSuite := range plan.SuiteList {
		f.WriteString("\n\n")
		if len(testSuite.comment) > 0 {
			WriteComment(testSuite.comment, f)
		}
		count, err := f.WriteString("---\n")
		if err != nil {
			return err
		}
		testMap := map[string]interface{}{testSuite.Name: testSuite.Tests}
		caseBytes, err := yaml.Marshal(testMap)
		if err != nil {
			return err
		}
		count, err = f.Write(caseBytes)
		if count != len(caseBytes) || err != nil {
			panic("writing test suite failed")
		}
	}
	return nil
}

func (plan *TestPlan) WriteResultToFile(path string) error {
	// We create a new test plan that just contain all the tests in one test suite.
	p := &TestPlan{}
	tc := &TestSuite{}
	// Test case name is the current time.
	tc.Name = time.Now().Format(time.RFC3339)
	p.SuiteMap = map[string]*TestSuite{tc.Name: tc}
	p.SuiteList = append(p.SuiteList, tc)

	for _, test := range plan.resultList {
		tc.Tests = append(tc.Tests, test)
	}
	return p.DumpToFile(path)
}

func ReadMetadata(path string) map[string]interface{} {
	var meta map[string]interface{}
	data, err := ioutil.ReadFile(filepath.Join(path, MetaFile))
	if err != nil {
		fmt.Println("File not found:", err)
	}
	err = yaml.Unmarshal([]byte(data), &meta)
	if err != nil {
		mqutil.Logger.Printf("error: %v", err)
	}
	return meta
}

func (plan *TestPlan) WriteFailures(path string) error {
	flags := os.O_CREATE | os.O_WRONLY
	var perms os.FileMode
	if plan.Repro {
		flags |= os.O_TRUNC
		perms = 0755
	} else {
		flags |= os.O_APPEND
		perms = 0644
	}
	f, err := os.OpenFile(filepath.Join(path, MeqaFails), flags, perms)
	defer f.Close()
	if err != nil {
		fmt.Println(err.Error())
	}
	d := json.NewEncoder(f)
	meta := ReadMetadata(path)
	for _, v := range plan.NewFailures {
		v.Meta = meta
		if err := d.Encode(v); err != nil {
			fmt.Println(err.Error())
			return err
		}
	}
	return nil
}

func (plan *TestPlan) LogErrors() {
	fmt.Print(mqutil.AQUA)
	fmt.Printf("------------------------SchemaMismatches-----------------------------\n")
	fmt.Print(mqutil.END)
	for _, t := range plan.resultList {
		if t.schemaError != nil {
			fmt.Print(mqutil.AQUA)
			fmt.Println("--------")
			fmt.Printf("%v: %v\n", t.Path, t.Name)
			fmt.Print(mqutil.END)
			fmt.Print(mqutil.YELLOW)
			fmt.Println(t.schemaError.Error())
			fmt.Print(mqutil.END)
		}
	}
	fmt.Print(mqutil.AQUA)
	fmt.Printf("-----------------------------Errors----------------------------------\n")
	fmt.Print(mqutil.END)
	for _, t := range plan.resultList {
		if t.responseError != nil {
			fmt.Print(mqutil.AQUA)
			fmt.Println("--------")
			fmt.Printf("%v: %v\n", t.Path, t.Name)
			fmt.Print(mqutil.END)
			fmt.Print(mqutil.RED)
			fmt.Println("Response Status Code:", t.resp.StatusCode())
			fmt.Println(t.responseError)
			fmt.Print(mqutil.END)
		}
	}
	fmt.Print(mqutil.AQUA)
	fmt.Println("---------------------------------------------------------------------")
	fmt.Print(mqutil.END)
}

func (plan *TestPlan) PrintSummary() {
	fmt.Print(mqutil.GREEN)
	fmt.Printf("%v: %v\n", mqutil.Passed, plan.ResultCounts[mqutil.Passed])
	fmt.Print(mqutil.RED)
	fmt.Printf("%v: %v\n", mqutil.Failed, plan.ResultCounts[mqutil.Failed])
	fmt.Print(mqutil.RED)
	fmt.Printf("Fuzz Fails: %v\n", len(plan.NewFailures))
	fmt.Print(mqutil.YELLOW)
	fmt.Printf("%v: %v\n", mqutil.Skipped, plan.ResultCounts[mqutil.Skipped])
	fmt.Printf("%v: %v\n", mqutil.SchemaMismatch, plan.ResultCounts[mqutil.SchemaMismatch])
	fmt.Print(mqutil.AQUA)
	fmt.Printf("%v: %v\n", mqutil.Total, plan.ResultCounts[mqutil.Total])
	fmt.Print(mqutil.END)
}

func (plan *TestPlan) Init(swagger *mqswag.Swagger, db *mqswag.DB) {
	plan.db = db
	plan.swagger = swagger
	plan.SuiteMap = make(map[string]*TestSuite)
	plan.SuiteList = nil
	plan.resultList = nil
}

// Run a named TestSuite in the test plan.
func (plan *TestPlan) Run(name string, parentTest *Test) (map[string]int, error) {
	tc, ok := plan.SuiteMap[name]
	resultCounts := make(map[string]int)
	if !ok || len(tc.Tests) == 0 {
		str := fmt.Sprintf("The following test suite is not found: %s", name)
		mqutil.Logger.Println(str)
		return resultCounts, errors.New(str)
	}
	tc.db = plan.db.CloneSchema()
	defer func() {
		tc.db = nil
	}()
	resultCounts[mqutil.Total] = len(tc.Tests)
	resultCounts[mqutil.Failed] = 0
	var tcErr error
	for i, test := range tc.Tests {
		if len(test.Ref) != 0 {
			test.Strict = tc.Strict
			resultCounts, err := plan.Run(test.Ref, test)
			if err != nil {
				return resultCounts, err
			}
			continue
		}

		if test.Name == MeqaInit {
			// Apply the parameters to the test suite.
			(&tc.TestParams).Copy(&test.TestParams)
			tc.Strict = test.Strict
			continue
		}

		dup := test.SchemaDuplicate()
		dup.Strict = tc.Strict
		if parentTest != nil {
			dup.CopyParent(parentTest)
		}
		dup.ResolveHistoryParameters(&History)
		History.Append(dup)
		if parentTest != nil {
			dup.Name = parentTest.Name // always inherit the name
		}
		payloads, err := dup.Run(tc)
		if payloads != nil && len(payloads) > 0 {
			if plan.NewFailures == nil {
				plan.NewFailures = make([]*mqswag.Payload, 0, len(payloads))
			}
			plan.NewFailures = append(plan.NewFailures, payloads...)
		}
		dup.err = err
		plan.resultList = append(plan.resultList, dup)
		if dup.schemaError != nil {
			resultCounts[mqutil.SchemaMismatch]++
		}
		if err != nil {
			resultCounts[mqutil.Failed]++
			if tcErr == nil {
				tcErr = err
			}
		} else {
			resultCounts[mqutil.Passed]++
		}
		if dup.Method == mqswag.MethodPost && len(dup.PathParams) == 0 && dup.resp.RawResponse.StatusCode >= 300 {
			fmt.Printf("Skipping %v tests...\n", len(tc.Tests)-i-1)
			resultCounts[mqutil.Skipped] += len(tc.Tests) - i - 1
			break
		}
	}
	return resultCounts, tcErr
}

// The current global TestPlan
var Current TestPlan

// TestHistory records the execution result of all the tests
type TestHistory struct {
	tests []*Test
	mutex sync.Mutex
}

// GetTest gets a test by its name
func (h *TestHistory) GetTest(name string) *Test {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	for i := len(h.tests) - 1; i >= 0; i-- {
		if h.tests[i].Name == name {
			return h.tests[i]
		}
	}
	return nil
}
func (h *TestHistory) Append(t *Test) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.tests = append(h.tests, t)
}

var History TestHistory

func init() {
	rand.Seed(int64(time.Now().Second()))
	resty.SetRedirectPolicy(resty.FlexibleRedirectPolicy(15))
}
