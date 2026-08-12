package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"unicode/utf8"

	"go.kenn.io/kwt/service"
)

var (
	errOperationEventInvalid  = errors.New("operation event payload is invalid")
	errOperationEventTooLarge = errors.New("operation event exceeds retained byte capacity")
)

const maxOperationDetailDepth = 64

type retainedOperationEvent struct {
	sequence uint64
	kind     service.OperationEventKind
	encoded  string
}

func encodeRetainedOperationEvent(
	event service.OperationEvent,
	remaining int,
) (retainedOperationEvent, error) {
	if event.Result != nil && !json.Valid(event.Result) {
		return retainedOperationEvent{}, fmt.Errorf("%w: result is not valid JSON", errOperationEventInvalid)
	}
	if remaining < 0 || !operationEventPayloadFits(event, remaining) {
		return retainedOperationEvent{}, errOperationEventTooLarge
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return retainedOperationEvent{}, errors.Join(errOperationEventInvalid, err)
	}
	if len(encoded) > remaining {
		return retainedOperationEvent{}, errOperationEventTooLarge
	}
	return retainedOperationEvent{
		sequence: event.Sequence,
		kind:     event.Kind,
		encoded:  string(encoded),
	}, nil
}

func operationEventPayloadFits(event service.OperationEvent, remaining int) bool {
	budget := remaining
	if !consumeJSONString(&budget, event.Message) {
		return false
	}
	if event.Result != nil && !consumeJSONBytes(&budget, len(event.Result)) {
		return false
	}
	if event.Prompt != nil {
		if !consumeJSONString(&budget, event.Prompt.ID) ||
			!consumeJSONString(&budget, event.Prompt.Kind) ||
			!consumeJSONString(&budget, event.Prompt.Message) ||
			!consumeJSONValue(&budget, reflect.ValueOf(event.Prompt.Details)) {
			return false
		}
	}
	if event.Failure != nil {
		if !consumeJSONString(&budget, string(event.Failure.Code)) ||
			!consumeJSONString(&budget, event.Failure.Message) ||
			!consumeJSONValue(&budget, reflect.ValueOf(event.Failure.Details)) {
			return false
		}
	}
	return true
}

func consumeJSONValue(budget *int, value reflect.Value) bool {
	return consumeJSONValueAtDepth(budget, value, 0)
}

func consumeJSONValueAtDepth(budget *int, value reflect.Value, depth int) bool {
	if depth > maxOperationDetailDepth {
		return false
	}
	if !value.IsValid() {
		return consumeJSONBytes(budget, 4)
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			return consumeJSONBytes(budget, 4)
		}
		return consumeJSONValueAtDepth(budget, value.Elem(), depth)
	}
	if value.Type() == reflect.TypeOf(json.RawMessage(nil)) {
		raw := value.Interface().(json.RawMessage)
		return json.Valid(raw) && consumeJSONBytes(budget, len(raw))
	}
	switch value.Kind() {
	case reflect.Bool:
		return consumeJSONBytes(budget, 5)
	case reflect.String:
		return consumeJSONString(budget, value.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Uintptr:
		return consumeJSONBytes(budget, 20)
	case reflect.Float32, reflect.Float64:
		floating := value.Float()
		return !math.IsNaN(floating) && !math.IsInf(floating, 0) && consumeJSONBytes(budget, 32)
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String || !consumeJSONBytes(budget, 2) {
			return false
		}
		iterator := value.MapRange()
		first := true
		for iterator.Next() {
			if !first && !consumeJSONBytes(budget, 1) {
				return false
			}
			first = false
			if !consumeJSONString(budget, iterator.Key().String()) ||
				!consumeJSONBytes(budget, 1) ||
				!consumeJSONValueAtDepth(budget, iterator.Value(), depth+1) {
				return false
			}
		}
		return true
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.IsNil() {
			return consumeJSONBytes(budget, 4)
		}
		if value.Type().Elem().Kind() == reflect.Uint8 {
			if value.Len() > *budget {
				return false
			}
			encodedBytes := 2 + ((value.Len() + 2) / 3 * 4)
			return consumeJSONBytes(budget, encodedBytes)
		}
		if !consumeJSONBytes(budget, 2) {
			return false
		}
		for index := 0; index < value.Len(); index++ {
			if index > 0 && !consumeJSONBytes(budget, 1) {
				return false
			}
			if !consumeJSONValueAtDepth(budget, value.Index(index), depth+1) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func consumeJSONString(budget *int, value string) bool {
	if !consumeJSONBytes(budget, 2) {
		return false
	}
	for index := 0; index < len(value); {
		character := value[index]
		if character < utf8.RuneSelf {
			width := 1
			switch character {
			case '\\', '"', '\b', '\f', '\n', '\r', '\t':
				width = 2
			default:
				if character < 0x20 || character == '<' || character == '>' || character == '&' {
					width = 6
				}
			}
			if !consumeJSONBytes(budget, width) {
				return false
			}
			index++
			continue
		}
		runeValue, runeSize := utf8.DecodeRuneInString(value[index:])
		encodedSize := runeSize
		if runeValue == utf8.RuneError && runeSize == 1 {
			encodedSize = 6
		} else if runeValue == '\u2028' || runeValue == '\u2029' {
			encodedSize = 6
		}
		if !consumeJSONBytes(budget, encodedSize) {
			return false
		}
		index += runeSize
	}
	return true
}

func consumeJSONBytes(budget *int, count int) bool {
	if count < 0 || count > *budget {
		return false
	}
	*budget -= count
	return true
}
