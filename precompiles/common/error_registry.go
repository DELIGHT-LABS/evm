package common

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

// ErrorMapping maps an error value, matched with errors.Is, to a Solidity
// custom error without arguments.
type ErrorMapping struct {
	Error         error
	SolidityError string
}

// NewErrorMapping declares an error-value mapping without changing its cause.
func NewErrorMapping(err error, solidityError string) ErrorMapping {
	return ErrorMapping{Error: err, SolidityError: solidityError}
}

type encodedErrorMapping struct {
	err  error
	data []byte
}

// ErrorRegistry holds ordered error-value mappings and their validated ABI
// payloads. Error values retain their identity; declarations and payloads are
// frozen at construction.
type ErrorRegistry struct {
	mappings []encodedErrorMapping
}

// NewErrorRegistry validates and encodes no-argument mappings once. Unlike the
// common packer's request-time Error(string) fallback, invalid declarations are
// initialization errors. Comparable duplicate error values are rejected; when
// custom Is methods overlap, the first matching declaration takes precedence.
func NewErrorRegistry(api abi.ABI, mappings ...ErrorMapping) (*ErrorRegistry, error) {
	if err := validateEffectiveABI(api); err != nil {
		return nil, err
	}
	registry := &ErrorRegistry{mappings: make([]encodedErrorMapping, 0, len(mappings))}
	seen := make(map[error]struct{}, len(mappings))
	for _, mapping := range mappings {
		value := reflect.ValueOf(mapping.Error)
		if !value.IsValid() {
			return nil, fmt.Errorf("error mapping requires a non-nil error value")
		}
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return nil, fmt.Errorf("error mapping requires a non-nil error value")
			}
		}
		if value.Comparable() {
			if _, exists := seen[mapping.Error]; exists {
				return nil, fmt.Errorf("duplicate error value mapping for %s", mapping.SolidityError)
			}
			seen[mapping.Error] = struct{}{}
		}
		definition, ok := api.Errors[mapping.SolidityError]
		if mapping.SolidityError == "" || !ok {
			return nil, fmt.Errorf("missing ABI error %s for error mapping", mapping.SolidityError)
		}
		if len(definition.Inputs) != 0 {
			return nil, fmt.Errorf("error mapping %s must be no-argument", mapping.SolidityError)
		}
		canonical := abi.NewError(mapping.SolidityError, nil)
		if definition.Name != canonical.Name || definition.Sig != canonical.Sig || definition.ID != canonical.ID {
			return nil, fmt.Errorf("error mapping requires canonical ABI error %s", canonical.Sig)
		}
		encoded := NewRevertWithSolidityError(api, mapping.SolidityError)
		var carrier RevertDataCarrier
		if !errors.As(encoded, &carrier) || !bytes.Equal(carrier.RevertData(), canonical.ID[:4]) {
			return nil, fmt.Errorf("failed to encode ABI error %s for error mapping", mapping.SolidityError)
		}
		registry.mappings = append(registry.mappings, encodedErrorMapping{
			err: mapping.Error, data: append([]byte(nil), carrier.RevertData()...),
		})
	}
	return registry, nil
}

// MustNewErrorRegistry constructs a registry or panics on invalid declarations.
func MustNewErrorRegistry(api abi.ABI, mappings ...ErrorMapping) *ErrorRegistry {
	registry, err := NewErrorRegistry(api, mappings...)
	if err != nil {
		panic(err)
	}
	return registry
}

// Translate returns whether an errors.Is match was found and a fresh carrier. A true match
// always contains the validated custom error, never a packing-failure fallback.
// Nil and unrecognized errors return false, nil. Callers own terminal-error
// policy and retain the original cause when adapting this payload for transport.
func (registry *ErrorRegistry) Translate(err error) (matched bool, revert error) {
	if err == nil {
		return false, nil
	}
	for _, mapping := range registry.mappings {
		if errors.Is(err, mapping.err) {
			return true, &RevertWithData{data: append([]byte(nil), mapping.data...)}
		}
	}
	return false, nil
}
