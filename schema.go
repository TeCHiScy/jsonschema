// Copyright 2017 Santhosh Kumar Tekuri. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jsonschema

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A Schema represents compiled version of json-schema.
type Schema struct {
	URL string // absolute url of the resource.
	Ptr string // json-pointer to schema. always starts with `#`.

	// type agnostic validations
	format    func(interface{}) bool
	Format    string
	Always    *bool         // always pass/fail. used when booleans are used as schemas in draft-07.
	Ref       *Schema       // reference to actual schema. if not nil, all the remaining fields are ignored.
	Types     []string      // allowed types.
	Constant  []interface{} // first element in slice is constant value. note: slice is used to capture nil constant.
	Enum      []interface{} // allowed values.
	enumError string        // error message for enum fail. captured here to avoid constructing error message every time.
	Not       *Schema
	AllOf     []*Schema
	AnyOf     []*Schema
	OneOf     []*Schema
	If        *Schema
	Then      *Schema // nil, when If is nil.
	Else      *Schema // nil, when If is nil.

	// object validations
	MinProperties         int      // -1 if not specified.
	MaxProperties         int      // -1 if not specified.
	Required              []string // list of required properties.
	Properties            map[string]*Schema
	PropertyNames         *Schema
	RegexProperties       bool // property names must be valid regex. used only in draft4 as workaround in metaschema.
	PatternProperties     map[*regexp.Regexp]*Schema
	AdditionalProperties  interface{}            // nil or bool or *Schema.
	Dependencies          map[string]interface{} // value is *Schema or []string.
	DependentRequired     map[string][]string
	DependentSchemas      map[string]*Schema
	UnevaluatedProperties *Schema

	// array validations
	MinItems         int // -1 if not specified.
	MaxItems         int // -1 if not specified.
	UniqueItems      bool
	Items            interface{} // nil or *Schema or []*Schema
	AdditionalItems  interface{} // nil or bool or *Schema.
	Contains         *Schema
	MinContains      int // 1 if not specified
	MaxContains      int // -1 if not specified
	UnevaluatedItems *Schema

	// string validations
	MinLength        int // -1 if not specified.
	MaxLength        int // -1 if not specified.
	Pattern          *regexp.Regexp
	ContentEncoding  string
	decoder          func(string) ([]byte, error)
	ContentMediaType string
	mediaType        func([]byte) error

	// number validators
	Minimum          *big.Rat
	ExclusiveMinimum *big.Rat
	Maximum          *big.Rat
	ExclusiveMaximum *big.Rat
	MultipleOf       *big.Rat

	// annotations. captured only when Compiler.ExtractAnnotations is true.
	Title       string
	Description string
	Default     interface{}
	ReadOnly    bool
	WriteOnly   bool
	Examples    []interface{}

	// user defined extensions
	Extensions map[string]interface{}
	extensions map[string]func(ctx ValidationContext, s interface{}, v interface{}) error
}

func newSchema(url, ptr string) *Schema {
	// fill with default values
	return &Schema{
		URL:           url,
		Ptr:           ptr,
		MinProperties: -1,
		MaxProperties: -1,
		MinItems:      -1,
		MaxItems:      -1,
		MinContains:   1,
		MaxContains:   -1,
		MinLength:     -1,
		MaxLength:     -1,
	}
}

// Compile parses json-schema at given url returns, if successful,
// a Schema object that can be used to match against json.
//
// Returned error can be *SchemaError
func Compile(url string) (*Schema, error) {
	return NewCompiler().Compile(url)
}

// MustCompile is like Compile but panics if the url cannot be compiled to *Schema.
// It simplifies safe initialization of global variables holding compiled Schemas.
func MustCompile(url string) *Schema {
	return NewCompiler().MustCompile(url)
}

// CompileString parses and compiles the given schema with given base url.
func CompileString(url, schema string) (*Schema, error) {
	c := NewCompiler()
	if err := c.AddResource(url, strings.NewReader(schema)); err != nil {
		return nil, err
	}
	return c.Compile(url)
}

// Validate validates the given json data, against the json-schema.
//
// Returned error can be *ValidationError.
func (s *Schema) Validate(r io.Reader) error {
	doc, err := DecodeJSON(r)
	if err != nil {
		return err
	}
	return s.ValidateInterface(doc)
}

// ValidateInterface validates given doc, against the json-schema.
//
// the doc must be the value decoded by json package using interface{} type.
// we recommend to use jsonschema.DecodeJSON(io.Reader) to decode JSON.
func (s *Schema) ValidateInterface(doc interface{}) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(InvalidJSONTypeError); ok {
				err = r.(InvalidJSONTypeError)
			} else {
				panic(r)
			}
		}
	}()
	if _, _, err := s.validate(doc); err != nil {
		finishSchemaContext(err, s)
		finishInstanceContext(err)
		return err
	}
	return nil
}

// validate validates given value v with this schema.
func (s *Schema) validate(v interface{}) (unevalProps map[string]struct{}, lastEvalItem int, err error) {
	lastEvalItem = -1

	// populate unevalProps
	if m, ok := v.(map[string]interface{}); ok {
		unevalProps = make(map[string]struct{})
		for pname := range m {
			unevalProps[pname] = struct{}{}
		}
	}
	validateWith := func(schema *Schema) error {
		ueProps, leItem, err := schema.validate(v)
		if err == nil {
			if leItem > lastEvalItem {
				lastEvalItem = leItem
			}
			for pname := range unevalProps {
				if _, ok := ueProps[pname]; !ok {
					delete(unevalProps, pname)
				}
			}
		}
		return err
	}

	if s.Always != nil {
		if !*s.Always {
			return unevalProps, lastEvalItem, validationError("", "always fail")
		}
		return unevalProps, lastEvalItem, nil
	}

	if len(s.Types) > 0 {
		vType := jsonType(v)
		matched := false
		for _, t := range s.Types {
			if vType == t {
				matched = true
				break
			} else if t == "integer" && vType == "number" {
				num, _ := new(big.Rat).SetString(fmt.Sprint(v))
				if num.IsInt() {
					matched = true
					break
				}
			}
		}
		if !matched {
			return unevalProps, lastEvalItem, validationError("type", "expected %s, but got %s", strings.Join(s.Types, " or "), vType)
		}
	}

	var errors []error

	if len(s.Constant) > 0 {
		if !equals(v, s.Constant[0]) {
			switch jsonType(s.Constant[0]) {
			case "object", "array":
				errors = append(errors, validationError("const", "const failed"))
			default:
				errors = append(errors, validationError("const", "value must be %#v", s.Constant[0]))
			}
		}
	}

	if len(s.Enum) > 0 {
		matched := false
		for _, item := range s.Enum {
			if equals(v, item) {
				matched = true
				break
			}
		}
		if !matched {
			errors = append(errors, validationError("enum", s.enumError))
		}
	}

	if s.format != nil && !s.format(v) {
		errors = append(errors, validationError("format", "%q is not valid %q", v, s.Format))
	}

	switch v := v.(type) {
	case map[string]interface{}:
		if s.MinProperties != -1 && len(v) < s.MinProperties {
			errors = append(errors, validationError("minProperties", "minimum %d properties allowed, but found %d properties", s.MinProperties, len(v)))
		}
		if s.MaxProperties != -1 && len(v) > s.MaxProperties {
			errors = append(errors, validationError("maxProperties", "maximum %d properties allowed, but found %d properties", s.MaxProperties, len(v)))
		}
		if len(s.Required) > 0 {
			var missing []string
			for _, pname := range s.Required {
				if _, ok := v[pname]; !ok {
					missing = append(missing, strconv.Quote(pname))
				}
			}
			if len(missing) > 0 {
				errors = append(errors, validationError("required", "missing properties: %s", strings.Join(missing, ", ")))
			}
		}

		if len(s.Properties) > 0 {
			for pname, pschema := range s.Properties {
				if pvalue, ok := v[pname]; ok {
					delete(unevalProps, pname)
					if _, _, err := pschema.validate(pvalue); err != nil {
						errors = append(errors, addContext(escape(pname), "properties/"+escape(pname), err))
					}
				}
			}
		}

		if s.PropertyNames != nil {
			for pname := range v {
				if _, _, err := s.PropertyNames.validate(pname); err != nil {
					errors = append(errors, addContext(escape(pname), "propertyNames", err))
				}
			}
		}

		if s.RegexProperties {
			for pname := range v {
				if !isRegex(pname) {
					errors = append(errors, validationError("", "patternProperty %q is not valid regex", pname))
				}
			}
		}
		for pattern, pschema := range s.PatternProperties {
			for pname, pvalue := range v {
				if pattern.MatchString(pname) {
					delete(unevalProps, pname)
					if _, _, err := pschema.validate(pvalue); err != nil {
						errors = append(errors, addContext(escape(pname), "patternProperties/"+escape(pattern.String()), err))
					}
				}
			}
		}
		if s.AdditionalProperties != nil {
			if allowed, ok := s.AdditionalProperties.(bool); ok {
				if !allowed && len(unevalProps) > 0 {
					pnames := make([]string, 0, len(unevalProps))
					for pname := range unevalProps {
						pnames = append(pnames, strconv.Quote(pname))
					}
					errors = append(errors, validationError("additionalProperties", "additionalProperties %s not allowed", strings.Join(pnames, ", ")))
				}
			} else {
				schema := s.AdditionalProperties.(*Schema)
				for pname := range unevalProps {
					if pvalue, ok := v[pname]; ok {
						if _, _, err := schema.validate(pvalue); err != nil {
							errors = append(errors, addContext(escape(pname), "additionalProperties", err))
						}
					}
				}
			}
			unevalProps = nil
		}
		for dname, dvalue := range s.Dependencies {
			if _, ok := v[dname]; ok {
				switch dvalue := dvalue.(type) {
				case *Schema:
					if err := validateWith(dvalue); err != nil {
						errors = append(errors, addContext("", "dependencies/"+escape(dname), err))
					}
				case []string:
					for i, pname := range dvalue {
						if _, ok := v[pname]; !ok {
							errors = append(errors, validationError("dependencies/"+escape(dname)+"/"+strconv.Itoa(i), "property %q is required, if %q property exists", pname, dname))
						}
					}
				}
			}
		}
		for dname, dvalue := range s.DependentRequired {
			if _, ok := v[dname]; ok {
				for i, pname := range dvalue {
					if _, ok := v[pname]; !ok {
						errors = append(errors, validationError("dependentRequired/"+escape(dname)+"/"+strconv.Itoa(i), "property %q is required, if %q property exists", pname, dname))
					}
				}
			}
		}
		for dname, dvalue := range s.DependentSchemas {
			if _, ok := v[dname]; ok {
				if err := validateWith(dvalue); err != nil {
					errors = append(errors, addContext("", "dependentSchemas/"+escape(dname), err))
				}
			}
		}

	case []interface{}:
		if s.MinItems != -1 && len(v) < s.MinItems {
			errors = append(errors, validationError("minItems", "minimum %d items allowed, but found %d items", s.MinItems, len(v)))
		}
		if s.MaxItems != -1 && len(v) > s.MaxItems {
			errors = append(errors, validationError("maxItems", "maximum %d items allowed, but found %d items", s.MaxItems, len(v)))
		}
		if s.UniqueItems {
			for i := 1; i < len(v); i++ {
				for j := 0; j < i; j++ {
					if equals(v[i], v[j]) {
						errors = append(errors, validationError("uniqueItems", "items at index %d and %d are equal", j, i))
					}
				}
			}
		}
		switch items := s.Items.(type) {
		case *Schema:
			for i, item := range v {
				if _, _, err := items.validate(item); err != nil {
					errors = append(errors, addContext(strconv.Itoa(i), "items", err))
				}
			}
			lastEvalItem = len(v) - 1
		case []*Schema:
			if additionalItems, ok := s.AdditionalItems.(bool); ok {
				if !additionalItems && len(v) > len(items) {
					errors = append(errors, validationError("additionalItems", "only %d items are allowed, but found %d items", len(items), len(v)))
				}
			}
			for i, item := range v {
				if i < len(items) {
					if _, _, err := items[i].validate(item); err != nil {
						errors = append(errors, addContext(strconv.Itoa(i), "items/"+strconv.Itoa(i), err))
					}
					lastEvalItem = i
				} else if sch, ok := s.AdditionalItems.(*Schema); ok {
					if _, _, err := sch.validate(item); err != nil {
						errors = append(errors, addContext(strconv.Itoa(i), "additionalItems", err))
					}
					lastEvalItem = i
				} else {
					break
				}
			}
			if additionalItems, ok := s.AdditionalItems.(bool); ok && additionalItems {
				lastEvalItem = len(v) - 1
			}
		}
		if s.Contains != nil && (s.MinContains != -1 || s.MaxContains != -1) {
			matched := 0
			var causes []error
			for i, item := range v {
				if _, _, err := s.Contains.validate(item); err != nil {
					causes = append(causes, addContext(strconv.Itoa(i), "", err))
				} else {
					matched++
				}
			}
			if s.MinContains != -1 && matched < s.MinContains {
				errors = append(errors, validationError("minContains", "valid must be >= %d, but got %d", s.MinContains, matched).add(causes...))
			}
			if s.MaxContains != -1 && matched > s.MaxContains {
				errors = append(errors, validationError("maxContains", "valid must be <= %d, but got %d", s.MaxContains, matched))
			}
		}

	case string:
		if s.MinLength != -1 || s.MaxLength != -1 {
			length := utf8.RuneCount([]byte(v))
			if s.MinLength != -1 && length < s.MinLength {
				errors = append(errors, validationError("minLength", "length must be >= %d, but got %d", s.MinLength, length))
			}
			if s.MaxLength != -1 && length > s.MaxLength {
				errors = append(errors, validationError("maxLength", "length must be <= %d, but got %d", s.MaxLength, length))
			}
		}
		if s.Pattern != nil && !s.Pattern.MatchString(v) {
			errors = append(errors, validationError("pattern", "does not match pattern %q", s.Pattern))
		}

		decoded := s.ContentEncoding == ""
		var content []byte
		if s.decoder != nil {
			b, err := s.decoder(v)
			if err != nil {
				errors = append(errors, validationError("contentEncoding", "%q is not %s encoded", v, s.ContentEncoding))
			} else {
				content, decoded = b, true
			}
		}
		if decoded && s.mediaType != nil {
			if s.decoder == nil {
				content = []byte(v)
			}
			if err := s.mediaType(content); err != nil {
				errors = append(errors, validationError("contentMediaType", "value is not of mediatype %q", s.ContentMediaType))
			}
		}

	case json.Number, float64, int, int32, int64:
		// lazy convert to *big.Rat to avoid allocation
		var numVal *big.Rat
		num := func() *big.Rat {
			if numVal == nil {
				numVal, _ = new(big.Rat).SetString(fmt.Sprint(v))
			}
			return numVal
		}

		if s.Minimum != nil && num().Cmp(s.Minimum) < 0 {
			errors = append(errors, validationError("minimum", "must be >= %v but found %v", s.Minimum, v))
		}
		if s.ExclusiveMinimum != nil && num().Cmp(s.ExclusiveMinimum) <= 0 {
			errors = append(errors, validationError("exclusiveMinimum", "must be > %v but found %v", s.ExclusiveMinimum, v))
		}
		if s.Maximum != nil && num().Cmp(s.Maximum) > 0 {
			errors = append(errors, validationError("maximum", "must be <= %v but found %v", s.Maximum, v))
		}
		if s.ExclusiveMaximum != nil && num().Cmp(s.ExclusiveMaximum) >= 0 {
			errors = append(errors, validationError("exclusiveMaximum", "must be < %v but found %v", s.ExclusiveMaximum, v))
		}
		if s.MultipleOf != nil {
			if q := new(big.Rat).Quo(num(), s.MultipleOf); !q.IsInt() {
				errors = append(errors, validationError("multipleOf", "%v not multipleOf %v", v, s.MultipleOf))
			}
		}
	}

	if s.Ref != nil {
		if err := validateWith(s.Ref); err != nil {
			finishSchemaContext(err, s.Ref)
			var refURL string
			if s.URL == s.Ref.URL {
				refURL = s.Ref.Ptr
			} else {
				refURL = s.Ref.URL + s.Ref.Ptr
			}
			return unevalProps, lastEvalItem, validationError("$ref", "doesn't validate with %q", refURL).add(err)
		}
	}

	if s.Not != nil && validateWith(s.Not) == nil {
		errors = append(errors, validationError("not", "not failed"))
	}

	for i, sch := range s.AllOf {
		if err := validateWith(sch); err != nil {
			errors = append(errors, validationError("allOf/"+strconv.Itoa(i), "allOf failed").add(err))
		}
	}

	if len(s.AnyOf) > 0 {
		matched := false
		var causes []error
		for i, sch := range s.AnyOf {
			if err := validateWith(sch); err == nil {
				matched = true
			} else {
				causes = append(causes, addContext("", strconv.Itoa(i), err))
			}
		}
		if !matched {
			errors = append(errors, validationError("anyOf", "anyOf failed").add(causes...))
		}
	}

	if len(s.OneOf) > 0 {
		matched := -1
		var causes []error
		for i, sch := range s.OneOf {
			if err := validateWith(sch); err == nil {
				if matched == -1 {
					matched = i
				} else {
					errors = append(errors, validationError("oneOf", "valid against schemas at indexes %d and %d", matched, i))
					break
				}
			} else {
				causes = append(causes, addContext("", strconv.Itoa(i), err))
			}
		}
		if matched == -1 {
			errors = append(errors, validationError("oneOf", "oneOf failed").add(causes...))
		}
	}

	if s.If != nil {
		if validateWith(s.If) == nil {
			if s.Then != nil {
				if err := validateWith(s.Then); err != nil {
					errors = append(errors, validationError("then", "if-then failed").add(err))
				}
			}
		} else {
			if s.Else != nil {
				if err := validateWith(s.Else); err != nil {
					errors = append(errors, validationError("else", "if-else failed").add(err))
				}
			}
		}
	}

	// unevaluated ---
	switch v := v.(type) {
	case map[string]interface{}:
		if s.UnevaluatedProperties != nil {
			for pname := range unevalProps {
				if pvalue, ok := v[pname]; ok {
					if _, _, err := s.UnevaluatedProperties.validate(pvalue); err != nil {
						errors = append(errors, addContext(escape(pname), "unevaluatedProperties", err))
					}
				}
			}
			unevalProps = nil
		}
	case []interface{}:
		if s.UnevaluatedItems != nil {
			for i, item := range v[lastEvalItem+1:] {
				if _, _, err := s.UnevaluatedItems.validate(item); err != nil {
					errors = append(errors, addContext(strconv.Itoa(lastEvalItem+1+i), "unevaluatedItems", err))
				}
			}
			lastEvalItem = len(v) - 1
		}
	}

	for name, cs := range s.Extensions {
		validate := s.extensions[name]
		if err := validate(ValidationContext{}, cs, v); err != nil {
			errors = append(errors, err)
		}
	}

	switch len(errors) {
	case 0:
		return unevalProps, lastEvalItem, nil
	case 1:
		return unevalProps, lastEvalItem, errors[0]
	default:
		return unevalProps, lastEvalItem, validationError("", "validation failed").add(errors...)
	}
}

// jsonType returns the json type of given value v.
//
// It panics if the given value is not valid json value
func jsonType(v interface{}) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number, float64, int, int32, int64:
		return "number"
	case string:
		return "string"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	}
	panic(InvalidJSONTypeError(fmt.Sprintf("%T", v)))
}

// equals tells if given two json values are equal or not.
func equals(v1, v2 interface{}) bool {
	v1Type := jsonType(v1)
	if v1Type != jsonType(v2) {
		return false
	}
	switch v1Type {
	case "array":
		arr1, arr2 := v1.([]interface{}), v2.([]interface{})
		if len(arr1) != len(arr2) {
			return false
		}
		for i := range arr1 {
			if !equals(arr1[i], arr2[i]) {
				return false
			}
		}
		return true
	case "object":
		obj1, obj2 := v1.(map[string]interface{}), v2.(map[string]interface{})
		if len(obj1) != len(obj2) {
			return false
		}
		for k, v1 := range obj1 {
			if v2, ok := obj2[k]; ok {
				if !equals(v1, v2) {
					return false
				}
			} else {
				return false
			}
		}
		return true
	case "number":
		num1, _ := new(big.Rat).SetString(fmt.Sprint(v1))
		num2, _ := new(big.Rat).SetString(fmt.Sprint(v2))
		return num1.Cmp(num2) == 0
	default:
		return v1 == v2
	}
}

// escape converts given token to valid json-pointer token
func escape(token string) string {
	token = strings.Replace(token, "~", "~0", -1)
	token = strings.Replace(token, "/", "~1", -1)
	return url.PathEscape(token)
}
