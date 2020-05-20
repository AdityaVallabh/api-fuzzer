package mqutil

const (
	FuzzPositive = "positive"
	FuzzDataType = "datatype"
	FuzzNegative = "negative"
)

type FuzzValue struct {
	Value    interface{}
	FuzzType string
}
