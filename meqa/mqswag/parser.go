// This package handles swagger.json file parsing
package mqswag

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/AdityaVallabh/swagger_meqa/meqa/mqutil"
	"gopkg.in/yaml.v2"

	spec "github.com/getkin/kin-openapi/openapi3"
	blns "github.com/minimaxir/big-list-of-naughty-strings/naughtystrings"

	"github.com/xeipuuv/gojsonschema"
)

// The type code we use in DAGNode's name. e.g. a node that represents definitions/User
// will have the name of "d:User"
const (
	TypeDef        = "d"
	TypeOp         = "o"
	FieldSeparator = "?"
)

const (
	MethodGet     = "get"
	MethodPut     = "put"
	MethodPost    = "post"
	MethodDelete  = "delete"
	MethodHead    = "head"
	MethodPatch   = "patch"
	MethodOptions = "options"
)

const (
	JsonResponse = "application/json"
)

var MethodAll []string = []string{MethodGet, MethodPut, MethodPost, MethodDelete, MethodHead, MethodPatch, MethodOptions}

const (
	FlagSuccess = 1 << iota
	FlagFail
	FlagWeak
)

const (
	DoneDataFile   = "mqdata.yml"
	UniqueKeysFile = "uniqueKeys.yml"
)

type MeqaTag struct {
	Class     string
	Property  string
	Operation string
	Flags     int64
}

type DatasetType struct {
	Positive map[string][]interface{} `yaml:"positive"`
	Negative map[string][]interface{} `yaml:"negative"`
}

type Payload struct {
	Endpoint string                 `json:"endpoint"`
	Method   string                 `json:"method"`
	Field    string                 `json:"field"`
	Value    interface{}            `json:"value"`
	FuzzType string                 `json:"fuzzType"`
	Expected string                 `json:"expected"`
	Actual   string                 `json:"actual"`
	Message  string                 `json:"message"`
	Meta     map[string]interface{} `json:"meta"`
}

type UniqueKeysStruct struct {
	Keys []string `yaml:"uniqueKeys"`
}

func (t *MeqaTag) Equals(o *MeqaTag) bool {
	return t.Class == o.Class && t.Property == o.Property && t.Operation == o.Operation
}

func (t *MeqaTag) ToString() string {
	str := "<meqa " + t.Class
	if len(t.Property) > 0 {
		str = str + "." + t.Property
	}
	if len(t.Operation) > 0 {
		str = str + "." + t.Operation
	}
	str = str + ">"
	return str
}

// GetMeqaTag extracts the <meqa > tags.
// Example. for  <meqa Pet.Name.update>, return Pet, Name, update
func GetMeqaTag(desc string) *MeqaTag {
	if len(desc) == 0 {
		return nil
	}
	re := regexp.MustCompile("<meqa *[/-~\\-]+\\.?[/-~\\-]*\\.?[a-zA-Z]* *[a-zA-Z,]* *>")
	ar := re.FindAllString(desc, -1)

	// TODO it's possible that we have multiple choices because the server can't be
	// certain. However, we only process one right now.
	if len(ar) == 0 {
		return nil
	}
	meqa := ar[0][6:]
	right := strings.IndexRune(meqa, '>')

	if right < 0 {
		mqutil.Logger.Printf("invalid meqa tag in description: %s", desc)
		return nil
	}
	meqa = strings.Trim(meqa[:right], " ")
	tags := strings.Split(meqa, " ")
	var flags int64
	var objtags string
	for _, t := range tags {
		if len(t) > 0 {
			if t == "success" {
				flags |= FlagSuccess
			} else if t == "fail" {
				flags |= FlagFail
			} else if t == "weak" {
				flags |= FlagWeak
			} else {
				objtags = t
			}
		}
	}

	contents := strings.Split(objtags, ".")
	switch len(contents) {
	case 1:
		return &MeqaTag{contents[0], "", "", flags}
	case 2:
		return &MeqaTag{contents[0], contents[1], "", flags}
	case 3:
		return &MeqaTag{contents[0], contents[1], contents[2], flags}
	default:
		mqutil.Logger.Printf("invalid meqa tag in description: %s", desc)
		return nil
	}
}

type Swagger spec.Swagger

var UniqueKeys map[string]bool

func ReadUniqueKeys(meqaPath string) error {
	var uniqueKeysStruct UniqueKeysStruct
	data, err := ioutil.ReadFile(filepath.Join(meqaPath, UniqueKeysFile))
	if err != nil {
		return err
	}
	err = yaml.Unmarshal([]byte(data), &uniqueKeysStruct)
	if err != nil {
		return err
	}
	UniqueKeys = make(map[string]bool)
	for _, key := range uniqueKeysStruct.Keys {
		UniqueKeys[key] = true
	}
	return nil
}

var Dataset, DoneData DatasetType

func filter(doneData, allData, dataset *map[string][]interface{}, batchSize int) {
	doneMap := make(map[string]map[interface{}]bool)
	for k, v := range *doneData {
		doneMap[k] = make(map[interface{}]bool)
		for _, i := range v {
			doneMap[k][i] = true
		}
	}
	allDone := true
	// Iterating through the dataset maximum twice in the case of completing a cycle.
	// In the first iteration, we realize we used up all the values in allData
	// In the second iteration, after resetting doneData, we pick the first "batchSize" values from allData
	for t := 0; t < 2 && allDone; t++ {
		for k, v := range *allData {
			for _, i := range v {
				if doneMap[k] == nil || !doneMap[k][i] {
					if (*dataset)[k] == nil {
						(*dataset) = make(map[string][]interface{})
					}
					if (*doneData)[k] == nil {
						(*doneData) = make(map[string][]interface{})
					}
					(*dataset)[k] = append((*dataset)[k], i)
					(*doneData)[k] = append((*doneData)[k], i)
					allDone = false // We have at least one value that hasn't been used yet
					if len((*dataset)[k]) >= batchSize {
						break
					}
				}
			}
		}
		// If doneData == allData, allDone == true and we didn't pick any values, so reset doneData and repeat
		// if not, we have some values and the loop breaks
		if allDone {
			*(doneData) = make(map[string][]interface{})
			doneMap = make(map[string]map[interface{}]bool)
		}
	}
}

func ReadDataset(datasetPath, meqaPath, fuzzMode string, batchSize int) error {
	readLocalDataset := func(datasetPath string) (DatasetType, error) {
		var dataset DatasetType
		data, err := ioutil.ReadFile(datasetPath)
		if err != nil {
			return dataset, err
		}
		err = yaml.Unmarshal([]byte(data), &dataset)
		return dataset, err
	}
	var AllData DatasetType
	var err error
	if datasetPath == "" {
		stringsList := blns.Unencoded()
		interfacesList := make([]interface{}, len(stringsList))
		for i, s := range stringsList {
			interfacesList[i] = s
		}
		if fuzzMode == mqutil.FuzzPositive {
			AllData.Positive = make(map[string][]interface{})
			AllData.Positive["string"] = interfacesList
		}
		if fuzzMode == mqutil.FuzzNegative {
			AllData.Negative = make(map[string][]interface{})
			AllData.Negative["string"] = interfacesList
		}
	} else {
		AllData, err = readLocalDataset(datasetPath)
		if err != nil {
			return err
		}
	}
	DoneData, err = readLocalDataset(filepath.Join(meqaPath, DoneDataFile))
	if err != nil {
		return err
	}
	if fuzzMode == mqutil.FuzzPositive {
		filter(&DoneData.Positive, &AllData.Positive, &Dataset.Positive, batchSize)
	}
	if fuzzMode == mqutil.FuzzNegative {
		filter(&DoneData.Negative, &AllData.Negative, &Dataset.Negative, batchSize)
	}
	return nil
}

func WriteDoneData(meqaPath string) error {
	data, err := yaml.Marshal(DoneData)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(filepath.Join(meqaPath, DoneDataFile), data, 0644)
}

// Init from a file
func CreateSwaggerFromURL(path string, meqaPath string) (*Swagger, error) {
	tmpPath := filepath.Join(meqaPath, ".meqatmp")
	os.Remove(tmpPath)
	tmpFile, err := os.Create(tmpPath)
	if err != nil {
		mqutil.Logger.Printf("can't access tmp file %s", tmpPath)
		return nil, err
	}
	defer os.Remove(tmpPath)

	// If input is yaml, transform to json
	var swaggerJsonPath string
	ar := strings.Split(path, ".")
	if ar[len(ar)-1] == "json" {
		swaggerJsonPath = path
	} else {
		yamlBytes, err := ioutil.ReadFile(path)
		if err != nil {
			mqutil.Logger.Printf("can't read file %s", path)
			return nil, err
		}
		jsonBytes, err := mqutil.YamlToJson(yamlBytes)
		if err != nil {
			mqutil.Logger.Printf("invalid yaml in file %s %v", path, err)
			return nil, err
		}
		_, err = tmpFile.Write(jsonBytes)
		if err != nil {
			mqutil.Logger.Printf("can't access tmp file %s", tmpPath)
			return nil, err
		}
		swaggerJsonPath = tmpPath
	}

	// specDoc, err := loads.Spec(swaggerJsonPath)
	spec, err := spec.NewSwaggerLoader().LoadSwaggerFromFile(swaggerJsonPath)
	if err != nil {
		mqutil.Logger.Printf("Can't open the following file: %s", path)
		mqutil.Logger.Println(err.Error())
		return nil, err
	}

	// log.Println("Would be serving:", specDoc.Spec().Info.Title)

	// return (*Swagger)(specDoc.Spec()), nil
	return (*Swagger)(spec), nil
}

func GetListFromFile(path string) (map[string]bool, error) {
	bytes, err := ioutil.ReadFile(path)
	if err != nil {
		mqutil.Logger.Printf("can't read file %s", path)
		return nil, err
	}
	lines := strings.Split(string(bytes), "\n")
	list := make(map[string]bool)
	for _, line := range lines {
		list[line] = true
	}
	return list, nil
}

// FindSchemaByName finds the schema defined by name in the swagger document.
func (swagger *Swagger) FindSchemaByName(name string) SchemaRef {
	schema, ok := swagger.Components.Schemas[name]
	if !ok {
		return SchemaRef{}
	}
	return (SchemaRef)(*schema)
}

// GetReferredSchema returns what the schema refers to, and nil if it doesn't refer to any.
func (swagger *Swagger) GetReferredSchema(schema SchemaRef) (string, SchemaRef, error) {
	tokens := strings.Split(schema.Ref, "/")
	if tokens[0] == "" {
		return "", SchemaRef{}, nil
	}
	if len(tokens) != 4 || tokens[1] != "components" || tokens[2] != "schemas" {
		return "", SchemaRef{}, mqutil.NewError(mqutil.ErrInvalid, fmt.Sprintf("Invalid reference: %s", schema.Ref))
	}
	referredSchema := swagger.FindSchemaByName(tokens[3])
	if referredSchema.Value == nil {
		return "", SchemaRef{}, mqutil.NewError(mqutil.ErrInvalid, fmt.Sprintf("Reference object not found: %s", schema.Ref))
	}
	return tokens[3], referredSchema, nil
}

// GetSchemaRootType gets the real object type fo the specified schema. It only returns meaningful
// data for object and array of object type of parameters. If the parameter is a basic type it returns
// nil
func (swagger *Swagger) GetSchemaRootType(schema SchemaRef, parentTag *MeqaTag) (*MeqaTag, SchemaRef) {
	tag := GetMeqaTag(schema.Value.Description)
	if tag == nil {
		tag = parentTag
	}
	referenceName, referredSchema, err := swagger.GetReferredSchema(schema)
	if err != nil {
		mqutil.Logger.Print(err)
		return nil, SchemaRef{}
	}
	if referredSchema.Value != nil {
		if tag == nil {
			tag = &MeqaTag{referenceName, "", "", 0}
		}
		return swagger.GetSchemaRootType(referredSchema, tag)
	}
	if len(schema.Value.Enum) != 0 {
		return nil, SchemaRef{}
	}
	if len(schema.Value.Type) == 0 {
		return nil, SchemaRef{}
	}
	if strings.Contains(schema.Value.Type, gojsonschema.TYPE_ARRAY) {
		itemSchema := (SchemaRef)(*schema.Value.Items)
		return swagger.GetSchemaRootType(itemSchema, tag)
	} else if strings.Contains(schema.Value.Type, gojsonschema.TYPE_OBJECT) {
		return tag, schema
	}
	return nil, SchemaRef{}
}

func GetDAGName(t string, n string, m string) string {
	return t + FieldSeparator + n + FieldSeparator + m
}

func AddDef(name string, schema *Schema, swagger *Swagger, dag *DAG) error {
	_, err := dag.NewNode(GetDAGName(TypeDef, name, ""), schema)
	if err != nil {
		// Name should be unique, so we don't expect this to fail.
		return err
	}
	return nil
}

// Dependencies keeps track of what this operation consumes and produces. It also keeps
// track of what the default dependency is when there is no tag. Default always point to
// either "Produces" or "Consumes"
type Dependencies struct {
	Produces map[string]interface{}
	Consumes map[string]interface{}
	Default  map[string]interface{}
	IsPost   bool
}

// CollectFromTag collects from the tag. It returns the classname being collected.
func (dep *Dependencies) CollectFromTag(tag *MeqaTag) string {
	if tag != nil && len(tag.Class) > 0 {
		// If there is a tag, and the tag's operation (which is always correct)
		// doesn't match what we want to collect, then skip.
		if len(tag.Operation) > 0 {
			if tag.Operation == MethodPost {
				dep.Produces[tag.Class] = 1
			} else {
				dep.Consumes[tag.Class] = 1
			}
		} else {
			dep.Default[tag.Class] = 1
		}
		return tag.Class
	}
	return ""
}

// collects all the objects referred to by the schema. All the object names are put into
// the specified map.
func CollectSchemaDependencies(schema SchemaRef, swagger *Swagger, dag *DAG, dep *Dependencies) error {
	iterFunc := func(swagger *Swagger, schemaName string, schema SchemaRef, context interface{}) error {
		collected := dep.CollectFromTag(GetMeqaTag(schema.Value.Description))
		if len(collected) == 0 && len(schemaName) > 0 {
			dep.Default[schemaName] = 1
		}

		return nil
	}

	return schema.Iterate(iterFunc, dep, swagger, false)
}

func CollectParamDependencies(params spec.Parameters, swagger *Swagger, dag *DAG, dep *Dependencies) error {
	defer func() { dep.Default = nil }()

	// the list of objects this method is producing that are specified through refs. We need to go through
	// them to find what objects they depend on - those objects will be out inputs (consumes)
	var inputsNeeded []string
	for _, param := range params {
		if dep.IsPost && (param.Value.In == "body" || param.Value.In == "formData") {
			dep.Default = dep.Produces
		} else {
			dep.Default = dep.Consumes
		}
		collected := dep.CollectFromTag(GetMeqaTag(param.Value.Description))

		if param.Value.Schema.Value != nil {
			schema := (SchemaRef)(*param.Value.Schema)
			if len(collected) == 0 {
				collected = dep.CollectFromTag(GetMeqaTag(schema.Value.Description))
			}
			if len(collected) > 0 {
				// Only try to collect addition info from the object schema if the object is not
				// inlined in the request. If it's inlined the we should continue to collect the
				// the input parameters from the schema itself.
				if !strings.Contains(schema.Value.Type, gojsonschema.TYPE_OBJECT) {
					inputsNeeded = append(inputsNeeded, collected)
					continue
				}
			} else {
				// Getting root type covers refs and arrays
				t, _ := swagger.GetSchemaRootType(schema, nil)
				if t != nil && len(t.Class) > 0 {
					dep.Default[t.Class] = 1
					inputsNeeded = append(inputsNeeded, t.Class)
					continue
				}
			}
			dep.Default = dep.Consumes
			err := CollectSchemaDependencies(schema, swagger, dag, dep)
			if err != nil {
				return err
			}
		}
	}

	// This heuristics is for the common case. When posting an object, the fields referred by it are input
	// parameters needed to create this object. More complicated cases are to be handled by tags.
	for _, name := range inputsNeeded {
		schema := swagger.FindSchemaByName(name)
		dep.Default = dep.Consumes
		err := CollectSchemaDependencies(schema, swagger, dag, dep)
		if err != nil {
			return err
		}
	}

	return nil
}

func CollectResponseDependencies(responses *spec.Responses, swagger *Swagger, dag *DAG, dep *Dependencies) error {
	if responses == nil {
		return nil
	}
	dep.Default = make(map[string]interface{}) // We don't assume by default anything so we throw Default away.
	defer func() { dep.Default = nil }()
	for respCodeS, respSpec := range *responses {
		respCode, _ := strconv.Atoi(respCodeS)
		collected := dep.CollectFromTag(GetMeqaTag(respSpec.Value.Description))
		if len(collected) > 0 {
			continue
		}
		if respSpec.Value != nil && respCode >= 200 && respCode < 300 {
			if respSpec.Value.Content != nil {
				err := CollectSchemaDependencies((SchemaRef)(*respSpec.Value.Content[JsonResponse].Schema), swagger, dag, dep)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

var methodWeight = map[string]int{
	MethodPost:    1,
	MethodGet:     2,
	MethodHead:    2,
	MethodOptions: 2,
	MethodPut:     3,
	MethodPatch:   3,
	MethodDelete:  4,
}

func AddOperation(pathName string, pathItem *spec.PathItem, method string, swagger *Swagger, dag *DAG, setPriority bool) error {
	op := pathItem.GetOperation(strings.ToUpper(method))
	if op == nil {
		return nil
	}

	var node *DAGNode
	var err error
	if setPriority {
		node = dag.NameMap[GetDAGName(TypeOp, pathName, method)]
	} else {
		node, err = dag.NewNode(GetDAGName(TypeOp, pathName, method), op)
		if err != nil {
			return err
		}
	}

	// The nodes that are part of outputs depends on this operation. The outputs are children.
	// We have to be careful here. Get operations will also return objects. For gets, the outputs
	// are children only if they are not part of input parameters.
	tag := GetMeqaTag(op.Description)
	dep := &Dependencies{}
	dep.Produces = make(map[string]interface{})
	dep.Consumes = make(map[string]interface{})

	if (tag != nil && tag.Operation == MethodPost) || ((tag == nil || len(tag.Operation) == 0) && method == MethodPost) {
		dep.IsPost = true
		if tag != nil && len(tag.Class) > 0 {
			dep.Produces[tag.Class] = 1
		}
	} else {
		dep.IsPost = false
	}

	// The order matters. At the end of CollectParamDependencies we collect the parameters
	// referred by the object we produce.
	err = CollectParamDependencies(op.Parameters, swagger, dag, dep)
	if err != nil {
		return err
	}

	err = CollectParamDependencies(pathItem.Parameters, swagger, dag, dep)
	if err != nil {
		return err
	}

	err = CollectResponseDependencies(&op.Responses, swagger, dag, dep)
	if err != nil {
		return err
	}

	// Get the highest parameter weight before we remove circular dependencies.
	if setPriority {
		for consumeName := range dep.Consumes {
			paramNode := dag.NameMap[GetDAGName(TypeDef, consumeName, "")]
			if node.Priority < paramNode.Weight {
				node.Priority = paramNode.Weight
			}
		}
		countParams := func(parameters spec.Parameters) int {
			numParams := 0
			for _, p := range parameters {
				if p.Value.In == "path" {
					numParams++
				}
			}
			return numParams
		}
		// Node's priority is the highest weight * 100 + the number of parameters * 10 + method weight
		m := method
		if tag != nil && len(tag.Operation) > 0 {
			m = tag.Operation
		}
		node.Priority = node.Priority*100 + (countParams(pathItem.Parameters)+countParams(op.Parameters))*10 + methodWeight[m]
		return nil
	}

	if dep.IsPost {
		// We are creating object. Some of the inputs will be from the same object, remove them from
		// the consumes field.
		for k := range dep.Produces {
			delete(dep.Consumes, k)
		}
	} else {
		// We are getting objects. We definitely depend on the parameters we consume.
		for k := range dep.Consumes {
			delete(dep.Produces, k)
		}
	}

	err = node.AddDependencies(dag, dep.Produces, true)
	if err != nil {
		return err
	}

	return node.AddDependencies(dag, dep.Consumes, false)
}

func (swagger *Swagger) AddToDAG(dag *DAG) error {
	// Add all definitions
	for name, schema := range swagger.Components.Schemas {
		schemaCopy := Schema(*schema.Value) // must make a copy first, the schema variable is reused in the loop scope
		err := AddDef(name, &schemaCopy, swagger, dag)
		if err != nil {
			return err
		}
	}
	// Add all children
	for name, schema := range swagger.Components.Schemas {
		node := dag.NameMap[GetDAGName(TypeDef, name, "")]
		collections := make(map[string]interface{})
		collectInner := func(swagger *Swagger, schemaName string, schema SchemaRef, context interface{}) error {
			if len(schemaName) > 0 && schemaName != name {
				collections[schemaName] = 1
			}
			return nil
		}
		((SchemaRef)(*schema)).Iterate(collectInner, nil, swagger, false)
		// The inner fields are the parents. The child depends on parents.
		node.AddDependencies(dag, collections, false)
	}

	// Add all operations
	for pathName, pathItem := range swagger.Paths {
		for _, method := range MethodAll {
			err := AddOperation(pathName, pathItem, method, swagger, dag, false)
			if err != nil {
				return err
			}
		}
	}
	// set priorities. This can only be done after the above, where all weights for all operations are set.
	for pathName, pathItem := range swagger.Paths {
		for _, method := range MethodAll {
			err := AddOperation(pathName, pathItem, method, swagger, dag, true)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
