package symphony

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/aws-controllers-k8s/symphony/api/v1alpha1"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/runtime"
)

var refs sync.Map
var refIndex atomic.Uint32

func init() {
	// Choose a sufficiently large number to avoid colliding with real customer int32s

	refIndex.Store(uint32(5))
}

func Ref[t any](expression string) t {

	data := *new(t)
	dataType := reflect.TypeOf(data)

	// We limit the size of the data to 4 bytes, as we need enough bits to track
	// references without colliding with user defined values

	switch any(data).(type) {
	case string:
		return interface{}(fmt.Sprintf("${%s}", expression)).(t)
	case int32:
		return interface{}(int32(100)).(t)
	default:
		if size := dataType.Size(); size <= 4 {
			panic(fmt.Sprintf("Unable to ref expression %q for type %s, size %d is less than 4 bytes", expression, dataType, size))
		}
	}

	ref := refIndex.Add(1)
	refs.Store(ref, expression)
	return data
}

func Definition[t any]() *v1alpha1.Definition {
	definition := &v1alpha1.Definition{}
	lo.Must0(json.Unmarshal(lo.Must(json.Marshal(new(t))), definition))
	return &v1alpha1.Definition{
		Spec: runtime.RawExtension{
			Raw: lo.Must(json.Marshal(definition.Spec)), // TODO, parse this appropriately
		},
		Status: runtime.RawExtension{
			Raw: lo.Must(json.Marshal(definition.Status)), // TODO, parse this appropriately
		},
	}
}

func Resource[t runtime.Object](name string, definition t) *v1alpha1.Resource {
	return &v1alpha1.Resource{
		Name: name,
		Definition: runtime.RawExtension{
			Raw:    lo.Must(json.Marshal(definition)),
			Object: definition,
		},
	}
}

func Compile(rg v1alpha1.ResourceGroup) {
	fmt.Printf(string(lo.Must(json.MarshalIndent(rg, "", "\t"))))
}
