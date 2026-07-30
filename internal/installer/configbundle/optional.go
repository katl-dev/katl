package configbundle

import (
	"encoding/json"
	"reflect"

	"gopkg.in/yaml.v3"
)

// Optional preserves whether an operator supplied a source field independently
// of the field's value. Resolved plans and persisted manifests use ordinary
// normalized values instead.
type Optional[T any] struct {
	value T
	set   bool
}

func (optional *Optional[T]) UnmarshalYAML(node *yaml.Node) error {
	var value T
	if err := node.Decode(&value); err != nil {
		return err
	}
	optional.value = value
	optional.set = true
	return nil
}

func (optional Optional[T]) MarshalYAML() (any, error) {
	if !optional.set {
		return nil, nil
	}
	return optional.value, nil
}

func (optional Optional[T]) MarshalJSON() ([]byte, error) {
	return json.Marshal(optional.value)
}

func (optional Optional[T]) IsZero() bool {
	return !optional.set
}

func (optional Optional[T]) Get() (T, bool) {
	return optional.value, optional.set
}

func (optional Optional[T]) Value() T {
	return optional.value
}

func Some[T any](value T) Optional[T] {
	return Optional[T]{value: value, set: true}
}

func supplied[T any](value T) Optional[T] {
	return Some(value)
}

func (Optional[T]) sourceOptionalValueType() reflect.Type {
	return reflect.TypeOf((*T)(nil)).Elem()
}

type sourceOptionalType interface {
	sourceOptionalValueType() reflect.Type
}

var sourceOptionalTypeInterface = reflect.TypeOf((*sourceOptionalType)(nil)).Elem()

func optionalValueType(t reflect.Type) (reflect.Type, bool) {
	if !t.Implements(sourceOptionalTypeInterface) {
		return nil, false
	}
	optional := reflect.Zero(t).Interface().(sourceOptionalType)
	return optional.sourceOptionalValueType(), true
}
