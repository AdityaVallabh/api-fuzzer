package mqutil

const (
	FuzzPositive = "positive"
	FuzzDataType = "datatype"
	FuzzNegative = "negative"
	FuzzAll      = "all"
)

type FuzzValue struct {
	Value    interface{}
	FuzzType string
}
