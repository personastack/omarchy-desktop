package wirejson

import (
	"bytes"
	"encoding/json"
	"io"
)

// ValidUniqueJSON accepts one complete JSON value with no duplicate object keys.
func ValidUniqueJSON(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if walk(decoder) != nil {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}

func walk(decoder *json.Decoder) error {
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
		keys := make(map[string]struct{})
		for decoder.More() {
			token, err = decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok {
				return io.ErrUnexpectedEOF
			}
			if _, exists := keys[key]; exists {
				return io.ErrUnexpectedEOF
			}
			keys[key] = struct{}{}
			if err := walk(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return io.ErrUnexpectedEOF
		}
		return nil
	case '[':
		for decoder.More() {
			if err := walk(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return io.ErrUnexpectedEOF
		}
		return nil
	default:
		return io.ErrUnexpectedEOF
	}
}
