package config

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Strict decoding for town config files (gt-y3pgh.1, D5). A config file
// decodes only when it is one JSON value whose every object key is declared
// by the Go type it decodes into. An unknown key is a parse error, not a
// silently ignored setting (G3-17): a misspelled or retired knob must be
// visible, because config that looks authoritative but does nothing teaches
// operators to distrust config.

// keyRef is one undeclared key: its dotted path and the decoder-style offset
// of its first byte (index+1, as ParseError.Offset expects).
type keyRef struct {
	path   string
	offset int64
}

// decodeJSONStrict decodes data into v and returns a *ParseError for a syntax
// error, a type error, trailing data, or any undeclared key.
func decodeJSONStrict(path string, data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err != nil {
		switch {
		case errors.Is(err, io.EOF):
			return &ParseError{Path: path, Err: errors.New("file is empty")}
		case errors.Is(err, io.ErrUnexpectedEOF):
			return newParseError(path, data, int64(len(data))+1, errors.New("unexpected end of file"))
		}
		return parseErrorFromJSON(path, data, err)
	}
	if next := skipSpace(data, dec.InputOffset()); next < int64(len(data)) {
		return newParseError(path, data, next+1, errors.New("data after the top-level value"))
	}

	keys, err := unknownJSONKeys(data, reflect.TypeOf(v))
	if err != nil {
		return parseErrorFromJSON(path, data, err)
	}
	if len(keys) == 0 {
		return nil
	}
	pe := newParseError(path, data, keys[0].offset, unknownKeysError(keys))
	for _, k := range keys {
		pe.Keys = append(pe.Keys, k.path)
	}
	return pe
}

func unknownKeysError(keys []keyRef) error {
	names := make([]string, 0, len(keys))
	for _, k := range keys {
		names = append(names, strconv.Quote(k.path))
	}
	if len(keys) == 1 {
		return fmt.Errorf("unknown key %s", names[0])
	}
	return fmt.Errorf("%d unknown keys: %s", len(keys), strings.Join(names, ", "))
}

func newParseError(path string, data []byte, offset int64, err error) *ParseError {
	pe := &ParseError{Path: path, Offset: offset, Err: err}
	if offset > 0 {
		pe.Line, pe.Column = lineColumn(data, offset)
	}
	return pe
}

func parseErrorFromJSON(path string, data []byte, err error) *ParseError {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	var offset int64
	switch {
	case errors.As(err, &syn):
		offset = syn.Offset
	case errors.As(err, &typ):
		offset = typ.Offset
	}
	return newParseError(path, data, offset, err)
}

func skipSpace(data []byte, from int64) int64 {
	for from < int64(len(data)) {
		switch data[from] {
		case ' ', '\t', '\r', '\n':
			from++
		default:
			return from
		}
	}
	return from
}

// unknownJSONKeys walks data's tokens alongside t and returns every object
// key that t does not declare. Map keys are free; json.RawMessage, interface
// values and types with their own unmarshaler are opaque.
func unknownJSONKeys(data []byte, t reflect.Type) ([]keyRef, error) {
	w := &keyWalker{data: data, dec: json.NewDecoder(bytes.NewReader(data))}
	if err := w.walk(t, ""); err != nil {
		return nil, err
	}
	return w.unknown, nil
}

type keyWalker struct {
	data    []byte
	dec     *json.Decoder
	unknown []keyRef
}

var (
	jsonUnmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
)

func opaque(t reflect.Type) bool {
	if t == nil || t.Kind() == reflect.Interface {
		return true
	}
	return reflect.PointerTo(t).Implements(jsonUnmarshalerType) ||
		reflect.PointerTo(t).Implements(textUnmarshalerType)
}

func (w *keyWalker) skipValue() error {
	var raw json.RawMessage
	return w.dec.Decode(&raw)
}

func (w *keyWalker) walk(t reflect.Type, path string) error {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if opaque(t) {
		return w.skipValue()
	}
	tok, err := w.dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil // scalar: the decode pass already checked its type
	}
	switch delim {
	case '{':
		return w.walkObject(t, path)
	case '[':
		return w.walkArray(t, path)
	}
	return nil
}

func (w *keyWalker) walkObject(t reflect.Type, path string) error {
	var fields map[string]reflect.Type
	if t.Kind() == reflect.Struct {
		fields = jsonFields(t)
	}
	for w.dec.More() {
		keyAt := skipSpace(w.data, w.dec.InputOffset())
		if keyAt < int64(len(w.data)) && w.data[keyAt] == ',' {
			keyAt = skipSpace(w.data, keyAt+1)
		}
		tok, err := w.dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		child := joinKey(path, key)
		var elem reflect.Type
		switch t.Kind() {
		case reflect.Struct:
			ft, known := lookupField(fields, key)
			if !known {
				w.unknown = append(w.unknown, keyRef{path: child, offset: keyAt + 1})
				if err := w.skipValue(); err != nil {
					return err
				}
				continue
			}
			elem = ft
		case reflect.Map:
			elem = t.Elem()
		default:
			if err := w.skipValue(); err != nil {
				return err
			}
			continue
		}
		if err := w.walk(elem, child); err != nil {
			return err
		}
	}
	_, err := w.dec.Token() // '}'
	return err
}

func (w *keyWalker) walkArray(t reflect.Type, path string) error {
	var elem reflect.Type
	if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		elem = t.Elem()
	}
	for i := 0; w.dec.More(); i++ {
		if elem == nil {
			if err := w.skipValue(); err != nil {
				return err
			}
			continue
		}
		if err := w.walk(elem, fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
	}
	_, err := w.dec.Token() // ']'
	return err
}

func joinKey(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// lookupField matches a key the way encoding/json assigns it: exact name
// first, then case-insensitively.
func lookupField(fields map[string]reflect.Type, key string) (reflect.Type, bool) {
	if ft, ok := fields[key]; ok {
		return ft, true
	}
	for name, ft := range fields {
		if strings.EqualFold(name, key) {
			return ft, true
		}
	}
	return nil, false
}

var jsonFieldCache sync.Map // reflect.Type -> map[string]reflect.Type

// jsonFields returns the JSON object keys a struct type declares, including
// the fields promoted from embedded structs, mapped to their types.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	if cached, ok := jsonFieldCache.Load(t); ok {
		return cached.(map[string]reflect.Type)
	}
	fields := make(map[string]reflect.Type)
	collectJSONFields(t, fields)
	jsonFieldCache.Store(t, fields)
	return fields
}

func collectJSONFields(t reflect.Type, into map[string]reflect.Type) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				collectJSONFields(ft, into)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		if _, taken := into[name]; !taken {
			into[name] = f.Type
		}
	}
}

var yamlLineRE = regexp.MustCompile(`line (\d+)`)
var yamlTypeSuffixRE = regexp.MustCompile(` in type .*$`)

// DecodeYAMLFile decodes one YAML document from data (read from path) into v,
// rejecting keys v does not declare. Failures are a *ParseError carrying the
// line yaml reports.
func DecodeYAMLFile(path string, data []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	err := dec.Decode(v)
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) {
		return &ParseError{Path: path, Err: errors.New("file is empty")}
	}
	var msgs []string
	var typ *yaml.TypeError
	if errors.As(err, &typ) {
		msgs = typ.Errors
	} else {
		msgs = []string{err.Error()}
	}
	for i, m := range msgs {
		msgs[i] = yamlTypeSuffixRE.ReplaceAllString(m, "")
	}
	pe := &ParseError{Path: path, Err: errors.New(strings.Join(msgs, "; "))}
	if m := yamlLineRE.FindStringSubmatch(msgs[0]); m != nil {
		pe.Line, _ = strconv.Atoi(m[1])
	}
	return pe
}
