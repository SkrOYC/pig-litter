package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const collaborationMaxJSONBytes = 8 << 20

func collaborationDecodeJSON(data []byte, value any) error {
	if len(data) > collaborationMaxJSONBytes {
		return errors.New("JSON document exceeds size limit")
	}
	keys := json.NewDecoder(bytes.NewReader(data))
	if err := collaborationCheckJSONValue(keys, 0); err != nil {
		return err
	}
	if _, err := keys.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

func collaborationCheckJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 128 {
		return errors.New("JSON nesting exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate or invalid JSON field %q", key)
			}
			seen[name] = true
			if err := collaborationCheckJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := collaborationCheckJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	_, err = decoder.Token()
	return err
}
