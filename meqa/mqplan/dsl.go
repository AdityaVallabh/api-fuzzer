package mqplan

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"

	"gopkg.in/resty.v1"

	"reflect"

	"github.com/AdityaVallabh/swagger_meqa/meqa/mqutil"

	"github.com/AdityaVallabh/swagger_meqa/meqa/mqswag"

	"encoding/json"

	spec "github.com/getkin/kin-openapi/openapi3"
	uuid "github.com/gofrs/uuid"
	"github.com/lucasjones/reggen"
	"github.com/xeipuuv/gojsonschema"
)

const (
	ExpectStatus = "status"
	ExpectBody   = "body"

	MaxRetries = 10

	StatusSuccess             = "success" // 2XX
	StatusCodeOk              = 200       // Create success
	StatusCodeNoResponse      = 204       // Delete success
	StatusCodeBadRequest      = 400       // Due to incorrect body parameters
	StatusCodeTooManyRequests = 429       // API rate limiting
)

type Datum interface{}

var (
	dataTypes = [...]string{
		gojsonschema.TYPE_BOOLEAN,
		gojsonschema.TYPE_INTEGER,
		gojsonschema.TYPE_NUMBER,
		gojsonschema.TYPE_STRING,
	}
)

func GetBaseURL(swagger *mqswag.Swagger) string {
	return swagger.Servers[0].URL
}

// Post: old - nil, new - the new object we create.
// Put, patch: old - the old object, new - the new one.
// Get: old - the old object, new - the one we get from the server.
// Delete: old - the existing object, new - nil.
type Comparison struct {
	old     map[string]interface{} // For put and patch, it stores the keys used in lookup
	oldUsed map[string]interface{} // The same as the old object but with only the fields that are used in the call.
	new     map[string]interface{}
	schema  mqswag.SchemaRef
}

func (comp *Comparison) GetMapByOp(op string) map[string]interface{} {
	if op == mqswag.MethodGet {
		if comp.old == nil {
			comp.old = make(map[string]interface{})
			comp.oldUsed = make(map[string]interface{})
		}
		return comp.old
	}
	if comp.new == nil {
		comp.new = make(map[string]interface{})
	}
	return comp.new
}

func (comp *Comparison) SetForOp(op string, key string, value interface{}) *Comparison {
	var newComp *Comparison
	m := comp.GetMapByOp(op)
	if _, ok := m[key]; !ok {
		m[key] = value
	} else {
		// exist already, create a new comparison object. This only happens when we update
		// an array of objects.
		newComp := &Comparison{nil, nil, nil, comp.schema}
		m = newComp.GetMapByOp(op)
		m[key] = value
	}
	if op == mqswag.MethodGet {
		comp.oldUsed[key] = m[key]
	}
	return newComp
}

// Test represents a test object in the DSL. Extra care needs to be taken to copy the
// Test before running it, because running it would change the parameter maps.
type Test struct {
	Name       string                 `yaml:"name,omitempty"`
	Path       string                 `yaml:"path,omitempty"`
	Method     string                 `yaml:"method,omitempty"`
	Ref        string                 `yaml:"ref,omitempty"`
	Expect     map[string]interface{} `yaml:"expect,omitempty"`
	Strict     bool                   `yaml:"strict,omitempty"`
	TestParams `yaml:",inline,omitempty" json:",inline,omitempty"`

	startTime time.Time
	stopTime  time.Time

	// Map of Object name (matching definitions) to the Comparison object.
	// This tracks what objects we need to add to DB at the end of test.
	comparisons map[string]([]*Comparison)
	sampleSpace map[string][]mqutil.FuzzValue

	tag   *mqswag.MeqaTag // The tag at the top level that describes the test
	db    *mqswag.DB
	suite *TestSuite
	op    *spec.Operation
	resp  *resty.Response
	err   error

	responseError interface{}
	schemaError   error
}

func (t *Test) Init(suite *TestSuite) {
	t.suite = suite
	if suite != nil {
		t.db = suite.plan.db
	}
	if len(t.Method) != 0 {
		t.Method = strings.ToLower(t.Method)
	}
	// if BodyParams is map, after unmarshal it is map[interface{}]
	var err error
	if t.BodyParams != nil {
		t.BodyParams, err = mqutil.YamlObjToJsonObj(t.BodyParams)
		if err != nil {
			mqutil.Logger.Print(err)
		}
	}
	if len(t.Expect) > 0 && t.Expect[ExpectBody] != nil {
		t.Expect[ExpectBody], err = mqutil.YamlObjToJsonObj(t.Expect[ExpectBody])
		if err != nil {
			mqutil.Logger.Print(err)
		}
	}
}

// Duplicate the schema with empty values
func (t *Test) SchemaDuplicate() *Test {
	test := *t
	test.Expect = mqutil.MapCopy(test.Expect)
	test.QueryParams = mqutil.MapCopy(test.QueryParams)
	test.FormParams = mqutil.MapCopy(test.FormParams)
	test.PathParams = mqutil.MapCopy(test.PathParams)
	test.HeaderParams = mqutil.MapCopy(test.HeaderParams)
	if m, ok := test.BodyParams.(map[string]interface{}); ok {
		test.BodyParams = mqutil.MapCopy(m)
	} else if a, ok := test.BodyParams.([]interface{}); ok {
		test.BodyParams = mqutil.ArrayCopy(a)
	}

	test.tag = nil
	test.op = nil
	test.resp = nil
	test.comparisons = make(map[string]([]*Comparison))
	test.sampleSpace = make(map[string][]mqutil.FuzzValue)
	test.err = nil
	test.db = test.suite.db

	return &test
}

// Copy the schema with values
func (t *Test) Duplicate() *Test {
	copyTest := t.SchemaDuplicate()
	copyTest.op = t.op
	for k, v := range t.comparisons {
		copyTest.comparisons[k] = make([]*Comparison, len(v))
		for i, c := range v {
			newC := &Comparison{
				old:     mqutil.MapCopy(c.old),
				oldUsed: mqutil.MapCopy(c.oldUsed),
				new:     mqutil.MapCopy(c.new),
				schema:  c.schema,
			}
			copyTest.comparisons[k][i] = newC
		}
	}
	return copyTest
}

func (t *Test) AddBasicComparison(tag *mqswag.MeqaTag, paramSpec *spec.Parameter, data interface{}) {
	if paramSpec == nil {
		return
	}
	if tag == nil || len(tag.Class) == 0 || len(tag.Property) == 0 {
		// No explicit tag. Info we have: t.Method, t.tag - indicate what operation we want to do.
		// t.path - indicate what object we want to operate on. We need to extrace the equivalent
		// of the tag. This is usually done on server, here we just make a simple effort.
		// TODO
		return
	}

	// It's possible that we are updating a list of objects. Due to the way we generate parameters,
	// we will always generate one complete object (both the lookup key and the new data) before we
	// move on to the next. If we find a collision, we know we need to create a new Comparison object.
	var op string
	if len(tag.Operation) > 0 {
		op = tag.Operation
	} else {
		if paramSpec.In == "formData" || paramSpec.In == "body" {
			op = mqswag.MethodPut
		} else {
			op = mqswag.MethodGet
		}
	}
	var comp *Comparison
	if t.comparisons[tag.Class] != nil && len(t.comparisons[tag.Class]) > 0 {
		comp = t.comparisons[tag.Class][len(t.comparisons[tag.Class])-1]
		newComp := comp.SetForOp(op, tag.Property, data)
		if newComp != nil {
			t.comparisons[tag.Class] = append(t.comparisons[tag.Class], newComp)
		}
		return
	}
	// Need to create a new compare object.
	comp = &Comparison{}
	comp.schema = t.db.Swagger.FindSchemaByName(tag.Class)
	comp.SetForOp(op, tag.Property, data)
	t.comparisons[tag.Class] = append(t.comparisons[tag.Class], comp)
}

func (t *Test) AddObjectComparison(tag *mqswag.MeqaTag, obj map[string]interface{}, schema mqswag.SchemaRef) {
	var class, method string
	if tag == nil {
		return
	}
	class = tag.Class
	method = tag.Operation

	// again the rule is that the child overrides the parent.
	if len(method) == 0 {
		if t.tag != nil && len(t.tag.Operation) > 0 {
			method = t.tag.Operation // At test level the tag indicates the real method
		} else {
			method = t.Method
		}
	}
	if len(class) == 0 {
		cl, s := t.db.FindMatchingSchema(obj)
		if s.Value == nil {
			mqutil.Logger.Printf("Can't find a known schema for obj %v", obj)
			return
		}
		class = cl
	}

	if method == mqswag.MethodPost || method == mqswag.MethodPut || method == mqswag.MethodPatch {
		// It's possible that we are updating a list of objects. Due to the way we generate parameters,
		// we will always generate one complete object (both the lookup key and the new data) before we
		// move on to the next.
		if t.comparisons[class] != nil && len(t.comparisons[class]) > 0 {
			last := t.comparisons[class][len(t.comparisons[class])-1]
			if last.new == nil {
				last.new = obj
				return
			}
			// During put, having an array of objects with just the "new" part is allowed. This
			// means the update key is included in the new object.
		}
		t.comparisons[class] = append(t.comparisons[class], &Comparison{nil, nil, obj, schema})
	} else {
		mqutil.Logger.Printf("unexpected: generating object %s for GET method.", class)
	}
}

func (t *Test) GetClientDB(className string, associations map[string]map[string]interface{}) []interface{} {
	var dbArray []interface{}
	if len(t.comparisons[className]) > 0 {
		for _, comp := range t.comparisons[className] {
			dbArray = append(dbArray, t.db.Find(className, comp.oldUsed, associations, mqutil.InterfaceEquals, -1)...)
		}
	} else {
		dbArray = t.db.Find(className, nil, associations, mqutil.InterfaceEquals, -1)
	}
	mqutil.Logger.Printf("got %d entries from db", len(dbArray))
	return dbArray
}

// Every object in the response must be in the client db
func (t *Test) ResponseInDb(className string, associations map[string]map[string]interface{}, resultArray []interface{}) error {
	fmt.Printf("... checking GET result against client. ")
	dbArray := t.GetClientDB(className, associations)
	numMiss := 0
	var missing string
	for _, entry := range resultArray {
		entryMap, _ := entry.(map[string]interface{})
		if entryMap == nil {
			// Server returned array of non-map types. Nothing for us to do. If the schema and server result doesn't
			// match we will catch that when we verify schema.
			continue
		}
		found := false
		for _, dbEntry := range dbArray {
			dbentryMap, _ := dbEntry.(map[string]interface{})
			if dbentryMap != nil && mqutil.InterfaceEquals(dbentryMap, entryMap) {
				found = true
				break
			}
		}
		if !found {
			b, _ := json.Marshal(entry)
			missing = string(b)
			numMiss++
		}
	}
	// Sometimes we GET an object directly without creating it.
	// In such cases, client db will be empty and we don't want the test to fail.
	if numMiss > 0 && len(dbArray) > 0 {
		var found string
		if len(dbArray) > 0 {
			b, _ := json.Marshal(dbArray[0])
			found = string(b)
		}
		fmt.Printf("Result not found on client. Fail\n")
		t.responseError = fmt.Sprintf("%v remote objects missing in client\nMissing:%v\nFound %v like:%v", numMiss, missing, len(dbArray), found)
		return mqutil.NewError(mqutil.ErrHttp, fmt.Sprintf("remote object not found in client\n"))
	}
	fmt.Printf("Success\n")
	return nil
}

// Every object in the client db must be in the response
func (t *Test) DbInResponse(className string, associations map[string]map[string]interface{}, resultArray []interface{}) error {
	fmt.Printf("... checking client objects against GET result. ")
	dbArray := t.GetClientDB(className, associations)
	numMiss := 0
	var missing string
	for _, dbEntry := range dbArray {
		dbentryMap, _ := dbEntry.(map[string]interface{})
		found := false
		for _, entry := range resultArray {
			entryMap, _ := entry.(map[string]interface{})
			if entryMap == nil {
				// Server returned array of non-map types. Nothing for us to do. If the schema and server result doesn't
				// match we will catch that when we verify schema.
				continue
			}
			if dbentryMap != nil && mqutil.InterfaceEquals(dbentryMap, entryMap) {
				found = true
				break
			}
		}
		if !found {
			b, _ := json.Marshal(dbEntry)
			missing = string(b)
			numMiss++
		}
	}
	if numMiss > 0 {
		fmt.Printf("Result not found on remote. Fail\n")
		t.responseError = fmt.Sprintf("%v local objects missing from a list of %v on remote\nMissing: %s\n", numMiss, len(resultArray), missing)
		return mqutil.NewError(mqutil.ErrHttp, fmt.Sprintf("client object not found in results returned\n"))
	}
	fmt.Printf("Success\n")
	return nil
}

// ProcessOneComparison processes one comparison object.
// Adds/Updates/Deletes objects from the client db
func (t *Test) ProcessOneComparison(className string, method string, comp *Comparison,
	associations map[string]map[string]interface{}, collection map[string][]interface{}) error {

	if method == mqswag.MethodDelete {
		mqutil.Logger.Printf("... deleting entry from client DB. Success\n")
		t.suite.db.Delete(className, comp.oldUsed, associations, mqutil.InterfaceEquals, 1)
		t.db.Delete(className, comp.oldUsed, associations, mqutil.InterfaceEquals, 1)
	} else if method == mqswag.MethodPost && comp.new != nil {
		mqutil.Logger.Printf("... adding entry to client DB. Success\n")
		t.suite.db.Insert(className, comp.new, associations)
		return t.db.Insert(className, comp.new, associations)
	} else if (method == mqswag.MethodPatch || method == mqswag.MethodPut) && comp.new != nil {
		mqutil.Logger.Printf("... updating entry in client DB. Success\n")
		t.suite.db.Update(className, comp.oldUsed, associations, mqutil.InterfaceEquals, comp.new, 1, method == mqswag.MethodPatch)
		count := t.db.Update(className, comp.oldUsed, associations, mqutil.InterfaceEquals, comp.new, 1, method == mqswag.MethodPatch)
		if count != 1 {
			mqutil.Logger.Printf("Failed to find any entry to update")
		}
	}
	return nil
}

func (t *Test) GetParam(path []string) interface{} {
	if len(path) < 2 {
		return nil
	}
	var section interface{}
	if path[0] == "pathParams" {
		section = t.PathParams
	} else if path[0] == "queryParams" {
		section = t.QueryParams
	} else if path[0] == "headerParams" {
		section = t.HeaderParams
	} else if path[0] == "formParams" {
		section = t.FormParams
	} else if path[0] == "bodyParams" {
		section = t.BodyParams
	} else if path[0] == "outputs" {
		section = t.Expect[ExpectBody]
	}

	topSection := section
	// First try the exact search. This only works if there is no
	// array on the search path.
	for _, field := range path[1:] {
		if section == nil {
			break
		}
		paramMap, ok := section.(map[string]interface{})
		if !ok {
			section = nil
			break
		}
		section = paramMap[field]
	}
	if section != nil {
		return section
	}

	// Search by iterate through all the maps. This only applies if we have only one
	// entry after the params section.
	if len(path[1:]) == 1 {
		var found interface{}
		callback := func(key string, value interface{}) error {
			if key == path[1] {
				found = value
				return mqutil.NewError(mqutil.ErrOK, "")
			}
			return nil
		}

		mqutil.IterateFieldsInInterface(topSection, callback)
		return found
	}
	return nil
}

// ProcessResult decodes the response from the server into a result array
func (t *Test) ProcessResult(resp *resty.Response) error {
	if t.err != nil {
		fmt.Printf("REST call hit the following error: %s\n", t.err.Error())
		return t.err
	}

	// useDefaultSpec := true
	t.resp = resp
	status := resp.StatusCode()
	var respSpec *spec.Response
	if t.op.Responses != nil {
		respObject, ok := t.op.Responses[fmt.Sprintf("%v", status)]
		if ok {
			respSpec = respObject.Value
			// useDefaultSpec = false
		} else if t.op.Responses.Default() != nil {
			respSpec = t.op.Responses.Default().Value
		}
	}
	if respSpec == nil {
		// Nothing specified in the swagger.json. Same as an empty spec.
		respSpec = &spec.Response{}
	}

	respBody := resp.Body()
	var respSchema mqswag.SchemaRef
	if respSpec.Content != nil && respSpec.Content[mqswag.JsonResponse] != nil && respSpec.Content[mqswag.JsonResponse].Schema != nil {
		respSchema = (mqswag.SchemaRef)(*(respSpec.Content[mqswag.JsonResponse].Schema))
	}
	var resultObj interface{}
	if len(respBody) > 0 {
		d := json.NewDecoder(bytes.NewReader(respBody))
		d.UseNumber()
		d.Decode(&resultObj)
	}

	// Before returning from this function, we should set the test's expect value to that
	// of actual result. This allows us to print out a result report that is the same format
	// as the test plan file, but with the expect value that reflects the current ground truth.
	setExpect := func() {
		t.Expect = make(map[string]interface{})
		t.Expect[ExpectStatus] = status
		if resultObj != nil {
			t.Expect[ExpectBody] = resultObj
		}
	}

	if mqutil.Verbose {
		fmt.Println("Verifying REST response")
	}
	// success based on return status
	success := (status >= 200 && status < 300)
	tag := mqswag.GetMeqaTag(respSpec.Description)
	if tag != nil && tag.Flags&mqswag.FlagFail != 0 {
		success = false
	}

	testSuccess := success
	var expectedStatus interface{} = StatusSuccess
	if t.Expect != nil && t.Expect[ExpectStatus] != nil {
		expectedStatus = t.Expect[ExpectStatus]
		if expectedStatus == "fail" {
			testSuccess = !success
		} else if expectedStatusNum, ok := expectedStatus.(int); ok {
			testSuccess = (expectedStatusNum == status)
		}
	}

	greenSuccess := fmt.Sprintf("%vSuccess%v", mqutil.GREEN, mqutil.END)
	redFail := fmt.Sprintf("%vFail%v", mqutil.RED, mqutil.END)
	yellowFail := fmt.Sprintf("%vFail%v", mqutil.YELLOW, mqutil.END)
	if testSuccess {
		fmt.Printf("... expecting status: %v got status: %d. %v API=%v Method=%v\n", expectedStatus, status, greenSuccess, t.Path, t.Method)
		if t.Expect != nil && t.Expect[ExpectBody] != nil {
			testSuccess = mqutil.InterfaceEquals(t.Expect[ExpectBody], resultObj)
			if testSuccess {
				fmt.Printf("... checking body against test's expect value. Success\n")
			} else {
				mqutil.InterfacePrint(map[string]interface{}{"... expecting body": t.Expect[ExpectBody]}, true)
				fmt.Printf("... actual response body: %s\n", respBody)
				fmt.Printf("... checking body against test's expect value. Fail\n")
				ejson, _ := json.Marshal(t.Expect[ExpectBody])
				setExpect()
				return mqutil.NewError(mqutil.ErrExpect, fmt.Sprintf(
					"=== test failed, expecting body: \n%s\ngot body:\n%s\n===", string(ejson), respBody))
			}
		}
	} else {
		t.responseError = resp
		fmt.Printf("... expecting status: %v got status: %d. %v\n", expectedStatus, status, redFail)
		setExpect()
		return mqutil.NewError(mqutil.ErrExpect, fmt.Sprintf("=== test failed, response code %d ===", status))
	}

	// Check if the response obj and respSchema match
	collection := make(map[string][]interface{})
	objMatchesSchema := false
	if resultObj != nil && respSchema.Value != nil {
		fmt.Printf("... verifying response against openapi schema. ")
		err := respSchema.Parses("", resultObj, collection, true, t.db.Swagger)
		if err != nil {
			fmt.Printf("%v\n", yellowFail)
			objMatchesSchema = true
			specBytes, _ := json.MarshalIndent(respSpec, "", "    ")
			mqutil.Logger.Printf("server response doesn't match swagger spec: \n%s", string(specBytes))
			t.schemaError = err
			if mqutil.Verbose {
				// fmt.Printf("... openapi response schema: %s\n", string(specBytes))
				// fmt.Printf("... response body: %s\n", string(respBody))
				fmt.Println(err.Error())
			}
			// schemaError is already set. No need to treat it as a hard failure
			setExpect()
			return nil

			// We ignore this if the response is success, and the spec we used is the default. This is a strong
			// indicator that the author didn't spec out all the success cases.
			/* Don't treat this as a hard failure for now. Too common.
			if !(useDefaultSpec && success) {
				setExpect()
				return err
			}
			*/
		} else {
			fmt.Printf("%v API=%v Method=%v\n", greenSuccess, t.Path, t.Method)
		}
	}
	if resultObj != nil && len(collection) == 0 && t.tag != nil && len(t.tag.Class) > 0 {
		// try to resolve collection from the hint on the operation's description field.
		classSchema := t.db.GetSchema(t.tag.Class)
		if classSchema.Value != nil {
			if classSchema.Matches(resultObj, t.db.Swagger) {
				collection[t.tag.Class] = append(collection[t.tag.Class], resultObj)
			} else {
				callback := func(value map[string]interface{}) error {
					if classSchema.Matches(value, t.db.Swagger) {
						collection[t.tag.Class] = append(collection[t.tag.Class], value)
					}
					return nil
				}
				mqutil.IterateMapsInInterface(resultObj, callback)
			}
		}
	}

	// Log some non-fatal errors.
	if respSchema.Value != nil {
		if len(respBody) > 0 {
			if resultObj == nil && !strings.Contains(respSchema.Value.Type, gojsonschema.TYPE_STRING) {
				specBytes, _ := json.MarshalIndent(respSpec, "", "    ")
				mqutil.Logger.Printf("server response doesn't match swagger spec: \n%s", string(specBytes))
			}
		} else {
			// If schema is an array, then not having a body is OK
			if !strings.Contains(respSchema.Value.Type, gojsonschema.TYPE_ARRAY) {
				mqutil.Logger.Printf("swagger.spec expects a non-empty response, but response body is actually empty")
			}
		}
	}
	if expectedStatus != StatusSuccess {
		setExpect()
		return nil
	}

	// Sometimes the server will return more data than requested. For instance, the server may generate
	// a uuid that the client didn't send. So for post and puts, we first go through the collection.
	// The assumption is that if the server returned an object of matching type, we should use that
	// instead of what the client thinks.
	method := t.Method
	if t.tag != nil && len(t.tag.Operation) > 0 {
		method = t.tag.Operation
	}

	// For posts, it's possible that the server has replaced certain fields (such as uuid). We should just
	// use the server's result.
	if method == mqswag.MethodPost || method == mqswag.MethodPut {
		var propertyCollection map[string][]interface{}
		if objMatchesSchema {
			propertyCollection = make(map[string][]interface{})
			respSchema.Parses("", resultObj, propertyCollection, false, t.db.Swagger)
		}

		for className, compList := range t.comparisons {
			if len(compList) > 0 && compList[0].new != nil {
				classList := collection[className]
				if len(classList) > 0 {
					// replace what we posted with what the server returned
					var newcompList []*Comparison
					for _, entry := range classList {
						c := Comparison{nil, nil, entry.(map[string]interface{}), mqswag.SchemaRef{}}
						newcompList = append(newcompList, &c)
					}
					collection[className] = nil
					// Check if the common fields between request and response match
					if t.Strict {
						for _, comp := range compList {
							found := false
							for _, entry := range classList {
								if mqutil.InterfaceEquals(comp.new, entry) {
									found = true
									break
								}
							}
							if !found {
								setExpect()
								b, _ := json.Marshal(comp.new)
								c, _ := json.Marshal(classList[0].(map[string]interface{}))
								fmt.Printf("... checking GET result against client DB. Result not found on client. Fail\n")
								t.responseError = fmt.Sprintf("Expected:\n%v\nFound:\n%v\n", string(b), string(c))
								if len(classList) > 1 {
									t.responseError = t.responseError.(string) + fmt.Sprintf("... and %v other objects.\n", len(classList)-1)
								}
								return mqutil.NewError(mqutil.ErrHttp, fmt.Sprintf("client object not found in results returned\n%s\n",
									string(b)))
							}
						}
					}
					t.comparisons[className] = newcompList
				} else if len(compList) == 1 {
					// When posting a single item, and the server returned fields that belong to the object,
					// replace that field with what the server returned.
					for k, v := range propertyCollection {
						keyAr := strings.Split(k, ".")
						if len(keyAr) == 2 && keyAr[0] == className && len(v) == 1 {
							compList[0].new[keyAr[1]] = v[0]
						}
					}
				}
			}
		}
		// Add all fields in the response (including extra ones like metadata) to comparisons list
		for className, resultArray := range collection {
			objTag := mqswag.MeqaTag{className, "", "", 0}
			for _, c := range resultArray {
				t.AddObjectComparison(&objTag, c.(map[string]interface{}), t.db.GetSchema(className))
			}
		}
	}

	// Associations are only for the objects that has one for each class and has an old object.
	associations := make(map[string]map[string]interface{})
	// TODO: Understand the need of asociations & comparisons and do things the right way
	// for className, compArray := range t.comparisons {
	// 	if len(compArray) == 1 && compArray[0].oldUsed != nil {
	// 		associations[className] = compArray[0].oldUsed
	// 	}
	// }

	if method == mqswag.MethodGet {
		// For gets, we process based on the result collection's class.
		for className, resultArray := range collection {
			var err error
			// If the path ends with the {objectID}, there's a good chance the response only contains one object
			// So the response must be found in the db
			// If it doesn't end with an {id} it's probably a listing endpoint and the db must be a subset of the response
			isGetSingleObject := t.Path[len(t.Path)-1] == '}'
			if isGetSingleObject {
				err = t.ResponseInDb(className, associations, resultArray)
			} else {
				err = t.DbInResponse(className, associations, resultArray)
			}
			if err != nil {
				setExpect()
				return err
			}
		}
	} else {
		// Add all comparisons to db
		for className, compArray := range t.comparisons {
			for _, c := range compArray {
				err := t.ProcessOneComparison(className, method, c, associations, collection)
				if err != nil {
					setExpect()
					return err
				}
			}
		}
	}

	if !t.Strict {
		// Add everything from the collection to the in-mem DB
		for className, classList := range collection {
			for _, entry := range classList {
				t.db.Insert(className, entry, associations)
			}
		}
	}

	setExpect()
	return nil
}

// SetRequestParameters sets the parameters. Returns the new request path.
func (t *Test) SetRequestParameters(req *resty.Request) string {
	files := make(map[string]string)
	for _, p := range t.op.Parameters {
		if p.Value.Schema.Value.Type == "file" && t.FormParams[p.Value.Name] != nil {
			// for swagger 2 file type can only be in formData
			if fname, ok := t.FormParams[p.Value.Name].(string); ok {
				files[p.Value.Name] = fname
				delete(t.FormParams, p.Value.Name)
			}
		}
	}
	if len(files) > 0 {
		req.SetFiles(files)
	}
	if len(t.FormParams) > 0 {
		req.SetFormData(mqutil.MapInterfaceToMapString(t.FormParams))
		mqutil.InterfacePrint(map[string]interface{}{"formParams": t.FormParams}, mqutil.Verbose)
	}
	for k, v := range files {
		t.FormParams[k] = v
	}

	if len(t.QueryParams) > 0 {
		req.SetQueryParams(mqutil.MapInterfaceToMapString(t.QueryParams))
		mqutil.InterfacePrint(map[string]interface{}{"queryParams": t.QueryParams}, mqutil.Verbose)
	}
	if t.BodyParams != nil {
		req.SetBody(t.BodyParams)
		mqutil.InterfacePrint(map[string]interface{}{"bodyParams": t.BodyParams}, mqutil.Verbose)
	}
	if len(t.HeaderParams) > 0 {
		req.SetHeaders(mqutil.MapInterfaceToMapString(t.HeaderParams))
		mqutil.InterfacePrint(map[string]interface{}{"headerParams": t.HeaderParams}, mqutil.Verbose)
	}
	path := t.Path
	if len(t.PathParams) > 0 {
		PathParamsStr := mqutil.MapInterfaceToMapString(t.PathParams)
		for k, v := range PathParamsStr {
			path = strings.Replace(path, "{"+k+"}", v, -1)
		}
		mqutil.InterfacePrint(map[string]interface{}{"pathParams": t.PathParams}, mqutil.Verbose)
	}
	return path
}

func (t *Test) CopyParent(parentTest *Test) {
	if parentTest != nil {
		t.Strict = parentTest.Strict
		t.Expect = mqutil.MapCopy(parentTest.Expect)
		t.QueryParams = mqutil.MapAdd(t.QueryParams, parentTest.QueryParams)
		t.PathParams = mqutil.MapAdd(t.PathParams, parentTest.PathParams)
		t.HeaderParams = mqutil.MapAdd(t.HeaderParams, parentTest.HeaderParams)
		t.FormParams = mqutil.MapAdd(t.FormParams, parentTest.FormParams)

		if parentTest.BodyParams != nil {
			if t.BodyParams == nil {
				t.BodyParams = parentTest.BodyParams
			} else {
				// replace with parent only if the types are the same
				if parentBodyMap, ok := parentTest.BodyParams.(map[string]interface{}); ok {
					if bodyMap, ok := t.BodyParams.(map[string]interface{}); ok {
						t.BodyParams = mqutil.MapCombine(bodyMap, parentBodyMap)
					}
				} else {
					// For non-map types, just replace with parent if they are the same type.
					if reflect.TypeOf(parentTest.BodyParams) == reflect.TypeOf(t.BodyParams) {
						t.BodyParams = parentTest.BodyParams
					}
				}
			}
		}
	}
}

// Often, requests require a unique field. Here, we generate and assign a new one randomly
func (t *Test) generateUniqueKeys(bodyMap map[string]interface{}) {
	bodySchema := (mqswag.SchemaRef)(*t.op.RequestBody.Value.Content[mqswag.JsonResponse].Schema)
	propSchemas := bodySchema.GetProperties(t.db.Swagger)
	for uniqueKey := range mqswag.UniqueKeys {
		if _, ok := propSchemas[uniqueKey]; ok {
			prop := (mqswag.SchemaRef)(*propSchemas[uniqueKey])
			bodyMap[uniqueKey], _ = generateString(prop, uniqueKey+"_")
		}
	}
}

// When fuzzing, a lot of assets are created which are cleaned up by this func
func deleteResource(t *Test) {
	var result map[string]interface{}
	json.Unmarshal([]byte(t.resp.String()), &result)
	if id, ok := result["id"]; ok {
		t.Method = mqswag.MethodDelete
		t.BodyParams = nil
		var dTest *Test
		// Pick the last delete test which is usually the one deleting the resource
		for _, test := range t.suite.Tests {
			if test.Method == mqswag.MethodDelete {
				dTest = test
			}
		}
		if dTest != nil {
			dTest = dTest.Duplicate()
			dTest.PathParams["id"] = id
			dTest.op = spec.NewOperation()
			// The object was added to db only if there was no error
			// So ensure there was no error, then prepare for delete
			if t.responseError == nil && t.schemaError == nil {
				dTest.comparisons = t.comparisons
				for _, v := range dTest.comparisons {
					for _, c := range v {
						c.oldUsed = c.new
					}
				}
			}
			dTest.Do()
		}
	}
}

func fuzzRequest(t *Test, copyMap map[string]interface{}, fkey, fuzzType string, failChan chan<- *mqswag.Payload, wg *sync.WaitGroup) {
	defer wg.Done()
	for _, cList := range t.comparisons {
		for _, c := range cList {
			c.new = mqutil.MapReplace(c.new, copyMap)
		}
	}
	t.BodyParams = mqutil.MapCombine(t.BodyParams.(map[string]interface{}), copyMap)
	expectStatus := StatusSuccess
	// If request is expected to fail, set the expectation to BadRequest
	if fuzzType == mqutil.FuzzDataType || fuzzType == mqutil.FuzzNegative {
		if t.Expect == nil {
			t.Expect = make(map[string]interface{})
		}
		t.Expect[ExpectStatus] = StatusCodeBadRequest
		t.Expect[ExpectBody] = nil
		expectStatus = fmt.Sprint(StatusCodeBadRequest)
	}
	err := t.Do()
	// If there were any errors, capture them in a payload object and send them over the failures channel
	if err != nil {
		payload := &mqswag.Payload{
			Endpoint: t.Path,
			Method:   t.Method,
			Field:    fkey,
			Value:    copyMap[fkey],
			FuzzType: fuzzType,
			Expected: expectStatus,
			Actual:   t.resp.Status(),
			Message:  t.resp.String(),
		}
		failChan <- payload
		b, err := json.Marshal(t.BodyParams)
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		fmt.Printf("Expecting %v; Got %v: %v\nRequest Body: %v\n", expectStatus, t.resp.StatusCode(), t.resp.String(), string(b))
	}
	// If the object was created, delete it
	if t.Method == mqswag.MethodPost && t.resp.StatusCode() == StatusCodeOk {
		deleteResource(t)
	}
}

// Returns a list of values to be fuzzed for each field in the request body
func (t *Test) getSamples() (map[string][]mqutil.FuzzValue, int) {
	samples, totalTests := make(map[string][]mqutil.FuzzValue), 1
	if t.BodyParams != nil {
		history := t.suite.plan.OldFailuresMap[t.Path][t.Method]
		if t.suite.plan.Repro {
			// Return values from previous failures
			for key, choices := range history {
				samples[key] = make([]mqutil.FuzzValue, 0, len(choices))
				for choice := range choices {
					samples[key] = append(samples[key], choice)
					totalTests++
				}
			}
		} else {
			// Skip any known failures and return new values
			for key, choices := range t.sampleSpace {
				samples[key] = make([]mqutil.FuzzValue, 0, len(choices))
				for _, choice := range choices {
					if !(history[key][choice]) {
						samples[key] = append(samples[key], choice)
						totalTests++
					}
				}
			}
		}
	}
	return samples, totalTests
}

// Executes the baseTest and if no error, proceeds to fuzzing
func fuzzTest(baseTest *Test) ([]*mqswag.Payload, error) {
	samples, totalTests := baseTest.getSamples()
	inParallel := baseTest.Method != mqswag.MethodPut
	fmt.Printf("Executing tests: %v\nIn parallel: %v\n", totalTests, inParallel)
	baseTest.suite.plan.ResultCounts[mqutil.FuzzTotal] += totalTests - 1 // Excluding baseTest
	baseCopy := baseTest.Duplicate()
	errPositive := baseTest.Do()
	failChan := make(chan *mqswag.Payload, totalTests)
	var wg sync.WaitGroup
	if errPositive == nil {
		// For each field, for each value
		// 1) duplicate the baseTest
		// 2) replace the unique fields with random values
		// 3) replace the chosen field's value
		// 4) make the request either parallely or sequentially
		for key, choices := range samples {
			for _, choice := range choices {
				testCopy := baseCopy.Duplicate()
				copyMap := mqutil.MapCopy(testCopy.BodyParams.(map[string]interface{}))
				testCopy.generateUniqueKeys(copyMap)
				copyMap[key] = choice.Value
				wg.Add(1)
				if inParallel {
					go fuzzRequest(testCopy, copyMap, key, choice.FuzzType, failChan, &wg)
				} else {
					fuzzRequest(testCopy, copyMap, key, choice.FuzzType, failChan, &wg)
				}
			}
		}
	}
	wg.Wait()
	close(failChan)
	payloads := make([]*mqswag.Payload, 0, len(failChan))
	for p := range failChan {
		payloads = append(payloads, p)
	}
	return payloads, errPositive
}

func (t *Test) Do() error {
	tc := t.suite
	req := resty.R()
	if len(tc.ApiToken) > 0 {
		req.SetAuthToken(tc.ApiToken)
	} else if len(tc.Username) > 0 {
		req.SetBasicAuth(tc.Username, tc.Password)
	}

	path := tc.plan.BaseURL + t.SetRequestParameters(req)
	var resp *resty.Response
	var err error
	fmt.Printf("calling API=%v Method=%v\n", t.Path, t.Method)
	for retries := 1; retries <= MaxRetries; retries++ {
		t.startTime = time.Now()
		switch t.Method {
		case mqswag.MethodGet:
			resp, err = req.Get(path)
		case mqswag.MethodPost:
			resp, err = req.Post(path)
		case mqswag.MethodPut:
			resp, err = req.Put(path)
		case mqswag.MethodDelete:
			resp, err = req.Delete(path)
		case mqswag.MethodPatch:
			resp, err = req.Patch(path)
		case mqswag.MethodHead:
			resp, err = req.Head(path)
		case mqswag.MethodOptions:
			resp, err = req.Options(path)
		default:
			return mqutil.NewError(mqutil.ErrInvalid, fmt.Sprintf("Unknown method in test %s: %v", t.Name, t.Method))
		}
		t.stopTime = time.Now()
		fmt.Printf("... call completed: %f seconds. Status=%v, API=%v Method=%v\n", t.stopTime.Sub(t.startTime).Seconds(), resp.StatusCode(), t.Path, t.Method)
		if err == nil && resp.StatusCode() != StatusCodeTooManyRequests {
			break
		}
		req.Header["Cookie"] = nil
		time.Sleep(time.Millisecond * (time.Duration)(1000+rand.Intn(3000*retries)))
	}
	if err != nil {
		t.err = mqutil.NewError(mqutil.ErrHttp, err.Error())
	} else {
		mqutil.Logger.Print(resp.Status())
		mqutil.Logger.Println(string(resp.Body()))
	}
	err = t.ProcessResult(resp)
	return err
}

// Run runs the test. Returns the test result.
func (t *Test) Run(tc *TestSuite) ([]*mqswag.Payload, error) {

	mqutil.Logger.Print("\n--- " + t.Name)
	fmt.Printf("\nRunning test case: %s\n", t.Name)
	err := t.ResolveParameters(tc)
	if err != nil {
		fmt.Printf("... Fail\n... %s\n", err.Error())
		return nil, err
	}
	return fuzzTest(t)
}

func StringParamsResolveWithHistory(str string, h *TestHistory) interface{} {
	begin := strings.Index(str, "{{")
	end := strings.Index(str, "}}")
	if end > begin {
		ar := strings.Split(strings.Trim(str[begin+2:end], " "), ".")
		if len(ar) < 3 {
			mqutil.Logger.Printf("invalid parameter: {{%s}}, the format is {{testName.paramSection.paramName}}, e.g. {{test1.output.id}}",
				str[begin+2:end])
			return nil
		}
		t := h.GetTest(ar[0])
		if t != nil {
			return t.GetParam(ar[1:])
		}
	}
	return nil
}

func MapParamsResolveWithHistory(paramMap map[string]interface{}, h *TestHistory) {
	for k, v := range paramMap {
		if str, ok := v.(string); ok {
			if result := StringParamsResolveWithHistory(str, h); result != nil {
				paramMap[k] = result
			}
		}
	}
}

func ArrayParamsResolveWithHistory(paramArray []interface{}, h *TestHistory) {
	for i, param := range paramArray {
		if paramMap, ok := param.(map[string]interface{}); ok {
			MapParamsResolveWithHistory(paramMap, h)
		} else if str, ok := param.(string); ok {
			if result := StringParamsResolveWithHistory(str, h); result != nil {
				paramArray[i] = result
			}
		}
	}
}

func (t *Test) ResolveHistoryParameters(h *TestHistory) {
	MapParamsResolveWithHistory(t.PathParams, h)
	MapParamsResolveWithHistory(t.FormParams, h)
	MapParamsResolveWithHistory(t.HeaderParams, h)
	MapParamsResolveWithHistory(t.QueryParams, h)
	if bodyMap, ok := t.BodyParams.(map[string]interface{}); ok {
		MapParamsResolveWithHistory(bodyMap, h)
	} else if bodyArray, ok := t.BodyParams.([]interface{}); ok {
		ArrayParamsResolveWithHistory(bodyArray, h)
	} else if bodyStr, ok := t.BodyParams.(string); ok {
		result := StringParamsResolveWithHistory(bodyStr, h)
		if result != nil {
			t.BodyParams = result
		}
	}
}

// ParamsAdd adds the parameters from src to dst if the param doesn't already exist on dst.
func ParamsAdd(dst spec.Parameters, src spec.Parameters) spec.Parameters {
	if len(dst) == 0 {
		return src
	}
	if len(src) == 0 {
		return dst
	}
	nameMap := make(map[string]int)
	for _, entry := range dst {
		nameMap[entry.Value.Name] = 1
	}
	for _, entry := range src {
		if nameMap[entry.Value.Name] != 1 {
			dst = append(dst, entry)
			nameMap[entry.Value.Name] = 1
		}
	}
	return dst
}

// ResolveParameters fullfills the parameters for the specified request using the in-mem DB.
// The resolved parameters will be added to test.Parameters map.
func (t *Test) ResolveParameters(tc *TestSuite) error {
	pathItem := t.db.Swagger.Paths[t.Path]
	t.op = GetOperationByMethod(pathItem, t.Method)
	if t.op == nil {
		return mqutil.NewError(mqutil.ErrNotFound, fmt.Sprintf("Path %s not found in swagger file", t.Path))
	}
	fmt.Printf("... resolving parameters.\n")

	// There can be parameters at the path level. We merge these with the operation parameters.
	t.op.Parameters = ParamsAdd(t.op.Parameters, pathItem.Parameters)

	t.tag = mqswag.GetMeqaTag(t.op.Description)

	var paramsMap map[string]interface{}
	var globalParamsMap map[string]interface{}
	var err error
	var genParam interface{}
	if t.op.RequestBody != nil {
		var bodyMap map[string]interface{}
		bodyIsMap := false
		if t.BodyParams != nil {
			bodyMap, bodyIsMap = t.BodyParams.(map[string]interface{})
		}
		if t.BodyParams != nil && !bodyIsMap {
			// Body is not map, we use it directly.
			bodySchema := (mqswag.SchemaRef)(*t.op.RequestBody.Value.Content[mqswag.JsonResponse].Schema)
			paramTag, schema := t.db.Swagger.GetSchemaRootType(bodySchema, mqswag.GetMeqaTag(bodySchema.Value.Description))
			if schema.Value != nil && paramTag != nil {
				objarray, _ := t.BodyParams.([]interface{})
				for _, obj := range objarray {
					objMap, ok := obj.(map[string]interface{})
					if ok {
						t.AddObjectComparison(paramTag, objMap, schema)
					}
				}
			}
			fmt.Print("provided\n")
		} else {
			if _, ok := t.op.RequestBody.Value.Content[mqswag.JsonResponse]; !ok {
				return mqutil.NewError(mqutil.ErrInvalid, "Unsupported type")
			}
			bodyParam := &spec.Parameter{Schema: t.op.RequestBody.Value.Content[mqswag.JsonResponse].Schema}
			genParam, err = t.GenerateParameter(bodyParam, t.db)
			if err != nil {
				return err
			}
			// Override generated params with static params if provided
			if genMap, genIsMap := genParam.(map[string]interface{}); genIsMap {
				if tcBodyMap, tcIsMap := tc.BodyParams.(map[string]interface{}); tcIsMap {
					bodyMap = mqutil.MapAdd(bodyMap, tcBodyMap)
				}
				t.BodyParams = mqutil.MapReplace(genMap, bodyMap)
			} else {
				t.BodyParams = genParam
			}
		}
	}
	for _, params := range t.op.Parameters {
		fmt.Printf("        %s (in %s): ", params.Value.Name, params.Value.In)
		switch params.Value.In {
		case "path":
			if t.PathParams == nil {
				t.PathParams = make(map[string]interface{})
			}
			paramsMap = t.PathParams
			globalParamsMap = tc.PathParams
		case "query":
			if t.QueryParams == nil {
				t.QueryParams = make(map[string]interface{})
			}
			paramsMap = t.QueryParams
			globalParamsMap = tc.QueryParams
		case "header":
			if t.HeaderParams == nil {
				t.HeaderParams = make(map[string]interface{})
			}
			paramsMap = t.HeaderParams
			globalParamsMap = tc.HeaderParams
		case "formData":
			if t.FormParams == nil {
				t.FormParams = make(map[string]interface{})
			}
			paramsMap = t.FormParams
			globalParamsMap = tc.FormParams
		}

		// If there is a parameter passed in, just use it. Otherwise generate one.
		_, inLocal := paramsMap[params.Value.Name]
		_, inGlobal := globalParamsMap[params.Value.Name]
		if !inLocal && inGlobal {
			paramsMap[params.Value.Name] = globalParamsMap[params.Value.Name]
		}
		if o, ok := paramsMap[params.Value.Name]; ok {
			if o != nil {
				t.AddBasicComparison(mqswag.GetMeqaTag(params.Value.Description), params.Value, paramsMap[params.Value.Name])
				fmt.Print("provided\n")
			} else {
				delete(paramsMap, params.Value.Name)
				fmt.Print("skipping\n")
			}
			continue
		}
		genParam, err = t.GenerateParameter(params.Value, t.db)
		if err != nil {
			return err
		}
		paramsMap[params.Value.Name] = genParam
	}
	return err
}

func GetOperationByMethod(item *spec.PathItem, method string) *spec.Operation {
	switch method {
	case mqswag.MethodGet:
		return item.Get
	case mqswag.MethodPost:
		return item.Post
	case mqswag.MethodPut:
		return item.Put
	case mqswag.MethodDelete:
		return item.Delete
	case mqswag.MethodPatch:
		return item.Patch
	case mqswag.MethodHead:
		return item.Head
	case mqswag.MethodOptions:
		return item.Options
	}
	return nil
}

// GenerateParameter generates paramter value based on the spec.
func (t *Test) GenerateParameter(paramSpec *spec.Parameter, db *mqswag.DB) (interface{}, error) {
	tag := mqswag.GetMeqaTag(paramSpec.Description)
	if paramSpec.Schema != nil {
		return t.GenerateSchema(paramSpec.Name, tag, (mqswag.SchemaRef)(*paramSpec.Schema), db, 3)
	}
	if len(paramSpec.Schema.Value.Enum) != 0 {
		fmt.Print("enum\n")
		return generateEnum(paramSpec.Schema.Value.Enum)
	}
	if len(paramSpec.Schema.Value.Type) == 0 {
		return nil, mqutil.NewError(mqutil.ErrInvalid, "Parameter doesn't have type")
	}

	// construct a full schema from simple ones
	schema := (mqswag.SchemaRef)(*paramSpec.Schema)
	if paramSpec.Schema.Value.Type == gojsonschema.TYPE_OBJECT {
		return t.generateObject("", tag, schema, db, 3)
	}
	if paramSpec.Schema.Value.Type == gojsonschema.TYPE_ARRAY {
		return t.generateArray("", tag, schema, db, 3)
	}

	return t.generateByType(schema, paramSpec.Name, tag, paramSpec, true)
}

// Two ways to get to generateByType
// 1) directly called from GenerateParameter, now we know the type is a parameter, and we want to add to comparison
// 2) called at bottom level, here we know the object will be added to comparison and not the type primitives.
func (t *Test) generateByType(s mqswag.SchemaRef, prefix string, parentTag *mqswag.MeqaTag, paramSpec *spec.Parameter, print bool) (interface{}, error) {
	tag := mqswag.GetMeqaTag(s.Value.Description)
	if tag == nil {
		tag = parentTag
	}
	if paramSpec != nil {
		if tag != nil && len(tag.Property) > 0 {
			// Try to get one from the comparison objects.
			for _, c := range t.comparisons[tag.Class] {
				if c.old != nil {
					c.oldUsed[tag.Property] = c.old[tag.Property]
					if print {
						fmt.Printf("found %s.%s\n", tag.Class, tag.Property)
					}
					return c.old[tag.Property], nil
				}
			}
			// Get one from in-mem db and populate the comparison structure.
			ar := t.suite.db.Find(tag.Class, nil, nil, mqswag.MatchAlways, 5)
			if len(ar) == 0 {
				ar = t.db.Find(tag.Class, nil, nil, mqswag.MatchAlways, 5)
			}
			if len(ar) > 0 {
				obj := ar[rand.Intn(len(ar))].(map[string]interface{})
				comp := &Comparison{obj, make(map[string]interface{}), nil, t.db.GetSchema(tag.Class)}
				comp.oldUsed[tag.Property] = comp.old[tag.Property]
				t.comparisons[tag.Class] = append(t.comparisons[tag.Class], comp)
				if print {
					fmt.Printf("found %s.%s\n", tag.Class, tag.Property)
				}
				return obj[tag.Property], nil
			}
		}
	}

	if len(s.Value.Type) != 0 {
		if print {
			fmt.Print("random\n")
		}
		result, err := generateValue(s.Value.Type, s, prefix)
		name := strings.ReplaceAll(prefix, "_", "")
		if result != nil && err == nil {
			t.AddBasicComparison(tag, paramSpec, result)
		}
		// Add positive cases to list possible values for the field
		if t.suite.plan.FuzzType == mqutil.FuzzPositive || t.suite.plan.FuzzType == mqutil.FuzzAll {
			for _, c := range mqswag.Dataset.Positive[s.Value.Type] {
				if mqswag.Validate(s, c) {
					fuzzValue := mqutil.FuzzValue{Value: c, FuzzType: mqutil.FuzzPositive}
					t.sampleSpace[name] = append(t.sampleSpace[name], fuzzValue)
				}
			}
		}
		// Add values of a different datatype than what's expected in the field
		if (t.suite.plan.FuzzType == mqutil.FuzzDataType || t.suite.plan.FuzzType == mqutil.FuzzAll) && s.Value.Type != gojsonschema.TYPE_STRING {
			s.Value.Format = ""
			for _, valueType := range dataTypes {
				if valueType != s.Value.Type {
					res, err := generateValue(valueType, s, prefix)
					fuzzValue := mqutil.FuzzValue{Value: res, FuzzType: mqutil.FuzzDataType}
					if res != nil && err == nil {
						t.sampleSpace[name] = append(t.sampleSpace[name], fuzzValue)
					}
				}
			}
		}
		// Add negative cases to the list of fuzzable values for the field
		if t.suite.plan.FuzzType == mqutil.FuzzNegative || t.suite.plan.FuzzType == mqutil.FuzzAll {
			for _, c := range mqswag.Dataset.Negative[s.Value.Type] {
				if !mqswag.Validate(s, c) {
					fuzzValue := mqutil.FuzzValue{Value: c, FuzzType: mqutil.FuzzNegative}
					t.sampleSpace[name] = append(t.sampleSpace[name], fuzzValue)
				}
			}
		}
		return result, err
	}

	return nil, mqutil.NewError(mqutil.ErrInvalid, fmt.Sprintf("unrecognized type: %s", s.Value.Type))
}

func generateValue(valueType string, s mqswag.SchemaRef, prefix string) (interface{}, error) {
	var result interface{}
	var err error
	switch valueType {
	case gojsonschema.TYPE_BOOLEAN:
		result, err = generateBool(s)
	case gojsonschema.TYPE_INTEGER:
		result, err = generateInt(s)
	case gojsonschema.TYPE_NUMBER:
		result, err = generateFloat(s)
	case gojsonschema.TYPE_STRING:
		result, err = generateString(s, prefix)
	case "file":
		return nil, errors.New("can not automatically upload a file, parameter of file type must be manually set\n")
	}
	return result, err
}

func generatePattern(format string) string {
	// https://gist.github.com/marcelotmelo/b67f58a08bee6c2468f8/
	dateRegex := "([0-9]+)-(0[1-9]|1[012])-(0[1-9]|[12][0-9]|3[01])"
	timeRegex := "([01][0-9]|2[0-3]):([0-5][0-9]):([0-5][0-9]|60)(\\.[0-9]+)?"
	timezoneRegex := "([\\+|\\-]([01][0-9]|2[0-3]):[0-5][0-9])"
	switch format {
	case "date-time":
		return fmt.Sprintf("^%s[Tt]%s(([Zz])|%s)$", dateRegex, timeRegex, timezoneRegex)
	case "date":
		return fmt.Sprintf("^%s$", dateRegex)
	case "uuid":
		return "^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$"
	case "email":
		return "^[a-z0-9]+@[a-z_]+?\\.[a-z]{2,3}$"
	default:
		return ""
	}
}

// RandomTime generate a random time in the range of [t, t + r).
func RandomTime(t time.Time, r time.Duration) time.Time {
	return t.Add(time.Duration(float64(r) * rand.Float64()))
}

// TODO we need to make it context aware. Based on different contexts we should generate different
// date ranges. Prefix is a prefix to use when generating strings. It's only used when there is
// no specified pattern in the swagger.json
func generateString(s mqswag.SchemaRef, prefix string) (string, error) {
	if len(s.Value.Pattern) == 0 {
		s.Value.Pattern = generatePattern(s.Value.Format)
	}
	if s.Value.Format == "date-time" {
		t := RandomTime(time.Now().UTC(), time.Hour*24*30)
		return t.Format(time.RFC3339), nil
	}
	if s.Value.Format == "date" {
		t := RandomTime(time.Now(), time.Hour*24*30)
		return t.Format("2006-01-02"), nil
	}
	if s.Value.Format == "uuid" {
		u, err := uuid.NewV4()
		return u.String(), err
	}

	// If no pattern is specified, we use the field name + some numbers as pattern
	var pattern string
	length := 0
	if len(s.Value.Pattern) != 0 {
		pattern = s.Value.Pattern
		length = len(s.Value.Pattern) * 2
	} else {
		pattern = prefix + "\\d{6,}"
		length = len(prefix) * 3
	}
	str, err := reggen.Generate(pattern, length)
	if err != nil {
		return "", mqutil.NewError(mqutil.ErrInvalid, err.Error())
	}

	if len(s.Value.Format) == 0 || s.Value.Format == "password" || s.Value.Format == "email" {
		return str, nil
	}
	if s.Value.Format == "byte" {
		return base64.StdEncoding.EncodeToString([]byte(str)), nil
	}
	if s.Value.Format == "binary" {
		return hex.EncodeToString([]byte(str)), nil
	}
	if s.Value.Format == "uri" || s.Value.Format == "url" {
		return "https://www.google.com/search?q=" + str, nil
	}
	return "", mqutil.NewError(mqutil.ErrInvalid, fmt.Sprintf("Invalid format string: %s", s.Value.Format))
}

func generateBool(s mqswag.SchemaRef) (interface{}, error) {
	return rand.Intn(2) == 0, nil
}

func generateFloat(s mqswag.SchemaRef) (float64, error) {
	var realmin float64
	// Set the minimum, if available
	if s.Value.Min != nil {
		realmin = *s.Value.Min
		if s.Value.ExclusiveMin {
			realmin += 0.01
		}
	}
	// Set the maximum, if available
	var realmax float64
	if s.Value.Max != nil {
		realmax = *s.Value.Max
		if s.Value.ExclusiveMax {
			realmax -= 0.01
		}
	}
	if realmin >= realmax {
		if s.Value.Min == nil && s.Value.Max == nil {
			realmin = -1.0
			realmax = 1.0
		} else if s.Value.Min != nil {
			realmax = realmin + math.Abs(realmin)
		} else if s.Value.Max != nil {
			realmin = realmax - math.Abs(realmax)
		} else {
			// both are present but conflicting
			return 0, mqutil.NewError(mqutil.ErrInvalid, fmt.Sprintf("specified min value %v is bigger than max %v",
				*s.Value.Min, *s.Value.Max))
		}
	}
	// If nothing is provided in the schema, choose a real number <10
	if realmin == 0 && realmax == 0 {
		realmax = 10.0
	}
	ret := rand.Float64()*(realmax-realmin) + realmin
	// Keep generating a new float until it cannot be casted to an integer
	// We want to generate floats like 7.01 and not 7.00
	for ret == float64(int(ret)) {
		ret = rand.Float64()*(realmax-realmin) + realmin
	}
	return ret, nil
}

func generateInt(s mqswag.SchemaRef) (int64, error) {
	// Give a default range if there isn't one
	if s.Value.Max == nil && s.Value.Min == nil {
		maxf := 1000000.0
		s.Value.Max = &maxf
	}
	f, err := generateFloat(s)
	if err != nil {
		return 0, err
	}
	i := int64(f)
	if s.Value.Min != nil && i <= int64(*s.Value.Min) {
		i++
	}
	return i, nil
}

func (t *Test) generateArray(name string, parentTag *mqswag.MeqaTag, schema mqswag.SchemaRef, db *mqswag.DB, level int) (interface{}, error) {
	var numItems int
	if schema.Value.MaxItems != nil || schema.Value.MinItems > 0 {
		var maxItems int = 10
		if schema.Value.MaxItems != nil {
			maxItems = int(*schema.Value.MaxItems)
			if maxItems <= 0 {
				maxItems = 1
			}
		}
		var minItems int
		minItems = int(schema.Value.MinItems)
		if minItems <= 0 {
			minItems = 1
		}
		maxDiff := maxItems - minItems
		if maxDiff <= 0 {
			maxDiff = 1
		}
		numItems = rand.Intn(int(maxDiff)) + minItems
	} else {
		numItems = rand.Intn(10)
	}
	if numItems <= 0 {
		numItems = 1
	}
	itemSchema := (mqswag.SchemaRef)(*schema.Value.Items)
	tag := mqswag.GetMeqaTag(schema.Value.Description)
	if tag == nil {
		tag = parentTag
	}

	var ar []interface{}
	var hash map[interface{}]interface{}
	if schema.Value.UniqueItems {
		hash = make(map[interface{}]interface{})
	}

	generateOneEntry := func() error {
		entry, err := t.GenerateSchema(name, tag, itemSchema, db, level)
		if err != nil {
			return err
		}
		if entry == nil {
			return nil
		}
		if hash != nil && hash[entry] != nil {
			return nil
		}
		ar = append(ar, entry)
		if hash != nil {
			hash[entry] = 1
		}
		return nil
	}

	// we only print one entry
	err := generateOneEntry()
	if err != nil {
		return nil, err
	}
	level = 0 // this will supress prints
	for i := 0; i < numItems; i++ {
		err = generateOneEntry()
		if err != nil {
			return nil, err
		}
	}
	return ar, nil
}

func (t *Test) generateObject(name string, parentTag *mqswag.MeqaTag, schema mqswag.SchemaRef, db *mqswag.DB, level int) (interface{}, error) {
	obj := make(map[string]interface{})
	var spaces string
	var nextLevel int
	if level > 0 {
		spaces = strings.Repeat("    ", level)
		nextLevel = level + 1
	}
	if level != 0 {
		fmt.Println("")
	}
	for k, v := range schema.Value.Properties {
		if level != 0 {
			fmt.Printf("%s%s . ", spaces, k)
		}
		if t.suite.BodyParams != nil {
			if o, ok := t.suite.BodyParams.(map[string]interface{})[k]; ok {
				if o != nil {
					obj[k] = o
					fmt.Println("found")
				} else {
					fmt.Println("skipping")
				}
				continue
			}
		}
		o, err := t.GenerateSchema(k+"_", nil, (mqswag.SchemaRef)(*v), db, nextLevel)
		if err != nil {
			return nil, err
		}
		obj[k] = o
	}

	tag := mqswag.GetMeqaTag(schema.Value.Description)
	if tag == nil {
		tag = parentTag
	}

	if tag != nil {
		t.AddObjectComparison(tag, obj, schema)
	}
	return obj, nil
}

// The parentTag passed in is what the higher level thinks this schema object should be.
func (t *Test) GenerateSchema(name string, parentTag *mqswag.MeqaTag, schema mqswag.SchemaRef, db *mqswag.DB, level int) (interface{}, error) {
	swagger := db.Swagger

	// The tag that's closest to the object takes priority, much like child class can override parent class.
	tag := mqswag.GetMeqaTag(schema.Value.Description)
	if tag == nil {
		tag = parentTag
	}

	// Deal with refs.
	referenceName, referredSchema, err := swagger.GetReferredSchema(schema)
	if err != nil {
		return nil, err
	}
	if referredSchema.Value != nil {
		if len(name) > 0 {
			// This the the field of an object. Instead of generating a new object, we try to get one
			// from the DB. If we can't find one, only then we generate a new one.
			found := t.suite.db.Find(referenceName, nil, nil, mqswag.MatchAlways, 1)
			if len(found) == 0 {
				found = t.db.Find(referenceName, nil, nil, mqswag.MatchAlways, 1)
			}
			if len(found) > 0 {
				if level != 0 {
					fmt.Printf("found %s\n", referenceName)
				}
				return found[0], nil
			}
		}
		return t.GenerateSchema(name, &mqswag.MeqaTag{referenceName, "", "", 0}, referredSchema, db, level)
	}

	if len(schema.Value.Enum) != 0 {
		if level != 0 {
			fmt.Print("enum\n")
		}
		return generateEnum(schema.Value.Enum)
	}

	if len(schema.Value.AllOf) > 0 {
		combined := make(map[string]interface{})
		discriminator := ""
		for _, s := range schema.Value.AllOf {
			m, err := t.GenerateSchema(name, nil, (mqswag.SchemaRef)(*s), db, level)
			if err != nil {
				return nil, err
			}
			if o, isMap := m.(map[string]interface{}); isMap {
				combined = mqutil.MapCombine(combined, o)
			} else {
				// We don't know how to combine AllOf properties of non-map types.
				jsonStr, _ := json.MarshalIndent(schema, "", "    ")
				return nil, mqutil.NewError(mqutil.ErrInvalid, fmt.Sprintf("can't combine AllOf schema that's not map: %s", jsonStr))
			}
			if s.Value.Discriminator != nil && len(s.Value.Discriminator.PropertyName) > 0 {
				discriminator = s.Value.Discriminator.PropertyName
			} else {
				// This is more common, the discriminator is in a common object referred from AllOf
				_, rs, _ := swagger.GetReferredSchema((mqswag.SchemaRef)(*s))
				if rs.Value != nil && rs.Value.Discriminator != nil && len(rs.Value.Discriminator.PropertyName) > 0 {
					discriminator = rs.Value.Discriminator.PropertyName
				}
			}
		}
		if len(discriminator) > 0 && len(tag.Class) > 0 {
			combined[discriminator] = tag.Class
		}
		// Add combined to the comparison under tag.
		t.AddObjectComparison(tag, combined, schema)
		return combined, nil
	}

	if len(schema.Value.Type) == 0 {
		// return nil, mqutil.NewError(mqutil.ErrInvalid, "Parameter doesn't have type")
		return t.generateObject(name, tag, schema, db, level)
	}
	if schema.Value.Type == gojsonschema.TYPE_OBJECT {
		return t.generateObject(name, tag, schema, db, level)
	}
	if schema.Value.Type == gojsonschema.TYPE_ARRAY {
		return t.generateArray(name, tag, schema, db, level)
	}

	return t.generateByType(schema, name, tag, nil, level != 0)
}

func generateEnum(e []interface{}) (interface{}, error) {
	return e[rand.Intn(len(e))], nil
}
