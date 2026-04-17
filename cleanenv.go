package cleanenv

import (
	"encoding"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
	"olympos.io/encoding/edn"
)

const (
	DefaultSeparator  = ","
	TagEnv            = "env"
	TagEnvLayout      = "env-layout"
	TagEnvDefault     = "env-default"
	TagEnvSeparator   = "env-separator"
	TagEnvDescription = "env-description"
	TagEnvUpd         = "env-upd"
	TagEnvRequired    = "env-required"
	TagEnvPrefix      = "env-prefix"
)

type (
	Setter interface {
		SetValue(string) error
	}
	Updater interface {
		Update() error
	}
	structMeta struct {
		envList                                 []string
		fieldValue                              reflect.Value
		separator, path, description, fieldName string
		defValue, layout                        *string
		updatable, required                     bool
	}
	parseFunc func(*reflect.Value, string, *string) error
)

var validStructs = map[reflect.Type]parseFunc{
	reflect.TypeFor[time.Time](): func(field *reflect.Value, value string, layout *string) error {
		l := time.RFC3339
		if layout != nil {
			l = *layout
		}
		val, err := time.Parse(l, value)
		if err != nil {
			return err
		}
		field.Set(reflect.ValueOf(val))
		return nil
	},
	reflect.TypeFor[url.URL](): func(field *reflect.Value, value string, _ *string) error {
		val, err := url.Parse(value)
		if err != nil {
			return err
		}
		field.Set(reflect.ValueOf(*val))
		return nil
	},
	reflect.TypeFor[*time.Location](): func(field *reflect.Value, value string, _ *string) error {
		loc, err := time.LoadLocation(value)
		if err != nil {
			return err
		}
		field.Set(reflect.ValueOf(loc))
		return nil
	},
}

func ReadConfig(path string, cfg any) error {
	err := parseFile(path, cfg)
	if err != nil {
		return err
	}
	return readEnvVars(cfg, false)
}

func ReadEnv(cfg any) error {
	return readEnvVars(cfg, false)
}

func UpdateEnv(cfg any) error {
	return readEnvVars(cfg, true)
}

func GetDescription(cfg any, headerText *string) (string, error) {
	meta, err := readStructMetadata(cfg)
	if err != nil {
		return "", err
	}
	var header string
	if headerText != nil {
		header = *headerText
	} else {
		header = "Environment variables:"
	}
	description := make([]string, 0)
	for _, m := range meta {
		if len(m.envList) == 0 {
			continue
		}
		for idx, env := range m.envList {
			elemDescription := fmt.Sprintf("\n  %s %s", env, m.fieldValue.Kind())
			if idx > 0 {
				elemDescription += fmt.Sprintf(" (alternative to %s)", m.envList[0])
			}
			elemDescription += fmt.Sprintf("\n    \t%s", m.description)
			if m.defValue != nil {
				elemDescription += fmt.Sprintf(" (default %q)", *m.defValue)
			}
			description = append(description, elemDescription)
		}
	}
	if len(description) == 0 {
		return "", nil
	}
	sort.Strings(description)
	return header + strings.Join(description, ""), nil
}

func Usage(cfg any, headerText *string, usageFuncs ...func()) func() {
	return FUsage(os.Stderr, cfg, headerText, usageFuncs...)
}

func FUsage(w io.Writer, cfg any, headerText *string, usageFuncs ...func()) func() {
	return func() {
		for _, fn := range usageFuncs {
			fn()
		}
		_ = flag.Usage
		text, err := GetDescription(cfg, headerText)
		if err != nil {
			return
		}
		if len(usageFuncs) > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, text)
	}
}

func ParseYAML(r io.Reader, str any) error {
	return yaml.NewDecoder(r).Decode(str)
}

func ParseJSON(r io.Reader, str any) error {
	return json.NewDecoder(r).Decode(str)
}

func ParseTOML(r io.Reader, str any) error {
	_, err := toml.NewDecoder(r).Decode(str)
	return err
}

func parseFile(path string, cfg any) error {
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_SYNC, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	default:
		return fmt.Errorf("file format '%s' doesn't supported by the parser", ext)
	case ".yaml", ".yml":
		err = ParseYAML(f, cfg)
	case ".json":
		err = ParseJSON(f, cfg)
	case ".toml":
		err = ParseTOML(f, cfg)
	case ".edn":
		err = parseEDN(f, cfg)
	case ".env":
		err = parseENV(f, cfg)
	}
	if err != nil {
		return fmt.Errorf("config file parsing error: %w", err)
	}
	return nil
}

func parseEDN(r io.Reader, str any) error {
	return edn.NewDecoder(r).Decode(str)
}

func parseENV(r io.Reader, _ any) error {
	vars, err := godotenv.Parse(r)
	if err != nil {
		return err
	}
	for env, val := range vars {
		if err = os.Setenv(env, val); err != nil {
			return fmt.Errorf("set environment: %w", err)
		}
	}

	return nil
}

func parseSlice(valueType reflect.Type, value string, sep string, layout *string) (*reflect.Value, error) {
	sliceValue := reflect.MakeSlice(valueType, 0, 0)
	if valueType.Elem().Kind() == reflect.Uint8 {
		sliceValue = reflect.ValueOf([]byte(value))
	} else if trimmed := strings.TrimSpace(value); trimmed != "" {
		values := strings.Split(trimmed, sep)
		sliceValue = reflect.MakeSlice(valueType, len(values), len(values))
		for i, val := range values {
			if err := parseValue(sliceValue.Index(i), val, sep, layout); err != nil {
				return nil, err
			}
		}
	}
	return &sliceValue, nil
}

func parseMap(valueType reflect.Type, value string, sep string, layout *string) (*reflect.Value, error) {
	mapValue := reflect.MakeMap(valueType)
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		pairs := strings.SplitSeq(trimmed, sep)
		for pair := range pairs {
			kvPair := strings.SplitN(pair, ":", 2)
			if len(kvPair) != 2 {
				return nil, fmt.Errorf("invalid map item: %q", pair)
			}
			k := reflect.New(valueType.Key()).Elem()
			if err := parseValue(k, kvPair[0], sep, layout); err != nil {
				return nil, err
			}
			v := reflect.New(valueType.Elem()).Elem()
			if err := parseValue(v, kvPair[1], sep, layout); err != nil {
				return nil, err
			}
			mapValue.SetMapIndex(k, v)
		}
	}
	return &mapValue, nil
}

func (sm *structMeta) isFieldValueZero() bool {
	return sm.fieldValue.IsZero()
}

func readStructMetadata(cfgRoot any) ([]structMeta, error) {
	type cfgNode struct {
		Val    any
		Prefix string
		Path   string
	}
	cfgStack := []cfgNode{{cfgRoot, "", ""}}
	metas := make([]structMeta, 0)
	for i := 0; i < len(cfgStack); i++ {
		s := reflect.ValueOf(cfgStack[i].Val)
		sPrefix := cfgStack[i].Prefix
		if s.Kind() == reflect.Pointer {
			s = s.Elem()
		}
		if s.Kind() != reflect.Struct {
			return nil, fmt.Errorf("wrong type %v", s.Kind())
		}
		typeInfo := s.Type()
		for idx := 0; idx < s.NumField(); idx++ {
			fType := typeInfo.Field(idx)
			var (
				defValue  *string
				layout    *string
				separator string
			)
			if fld := s.Field(idx); fld.Kind() == reflect.Struct {
				if !fld.CanInterface() {
					continue
				}
				_, implementsSetter := fld.Addr().Interface().(Setter)
				if _, found := validStructs[fld.Type()]; !found && !implementsSetter {
					prefix, _ := fType.Tag.Lookup(TagEnvPrefix)
					cfgStack = append(cfgStack, cfgNode{
						Val:    fld.Addr().Interface(),
						Prefix: sPrefix + prefix,
						Path:   fmt.Sprintf("%s%s.", cfgStack[i].Path, fType.Name),
					})
					continue
				}
				if l, ok := fType.Tag.Lookup(TagEnvLayout); ok {
					layout = &l
				}
			}
			if !s.Field(idx).CanSet() {
				continue
			}
			if def, ok := fType.Tag.Lookup(TagEnvDefault); ok {
				defValue = &def
			}
			if sep, ok := fType.Tag.Lookup(TagEnvSeparator); ok {
				separator = sep
			} else {
				separator = DefaultSeparator
			}
			_, upd := fType.Tag.Lookup(TagEnvUpd)
			_, required := fType.Tag.Lookup(TagEnvRequired)
			envList := make([]string, 0)
			if envs, ok := fType.Tag.Lookup(TagEnv); ok && envs != "" {
				envList = strings.Split(envs, DefaultSeparator)
				if sPrefix != "" {
					for j := range envList {
						envList[j] = sPrefix + envList[j]
					}
				}
			}
			metas = append(metas, structMeta{
				envList:     envList,
				fieldName:   s.Type().Field(idx).Name,
				fieldValue:  s.Field(idx),
				defValue:    defValue,
				layout:      layout,
				separator:   separator,
				description: fType.Tag.Get(TagEnvDescription),
				updatable:   upd,
				required:    required,
				path:        cfgStack[i].Path,
			})
		}

	}
	return metas, nil
}

func readEnvVars(cfg any, update bool) error {
	metaInfo, err := readStructMetadata(cfg)
	if err != nil {
		return err
	}
	if updater, ok := cfg.(Updater); ok {
		if err = updater.Update(); err != nil {
			return err
		}
	}
	for _, meta := range metaInfo {
		if update && !meta.updatable {
			continue
		}
		var rawValue *string
		for _, env := range meta.envList {
			if value, ok := os.LookupEnv(env); ok {
				rawValue = &value
				break
			}
		}
		var envName string
		if len(meta.envList) > 0 {
			envName = meta.envList[0]
		}
		if rawValue == nil && meta.required && meta.isFieldValueZero() {
			return fmt.Errorf("field %q is required but the value is not provided",
				meta.path+meta.fieldName,
			)
		}
		if rawValue == nil && meta.isFieldValueZero() {
			rawValue = meta.defValue
		}
		if rawValue == nil {
			continue
		}
		if err = parseValue(meta.fieldValue, *rawValue, meta.separator, meta.layout); err != nil {
			return fmt.Errorf("parsing field %q env %q: %v",
				meta.path+meta.fieldName, envName, err,
			)
		}
	}
	return nil
}

func parseValue(field reflect.Value, value, sep string, layout *string) error {
	valueType := field.Type()
	if structParser, found := validStructs[valueType]; found {
		return structParser(&field, value, layout)
	}
	if field.CanInterface() {
		if ct, ok := field.Interface().(encoding.TextUnmarshaler); ok {
			return ct.UnmarshalText([]byte(value))
		} else if ctp, ok := field.Addr().Interface().(encoding.TextUnmarshaler); ok {
			return ctp.UnmarshalText([]byte(value))
		}
		if cs, ok := field.Interface().(Setter); ok {
			return cs.SetValue(value)
		} else if csp, ok := field.Addr().Interface().(Setter); ok {
			return csp.SetValue(value)
		}
	}
	switch valueType.Kind() {
	default:
		return fmt.Errorf("unsupported type %v", valueType.Kind())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32:
		num, err := strconv.ParseInt(value, 0, valueType.Bits())
		if err != nil {
			return err
		}
		field.SetInt(num)
	case reflect.Int64:
		if valueType == reflect.TypeFor[time.Duration]() {
			d, err := time.ParseDuration(value)
			if err != nil {
				return err
			}
			field.SetInt(int64(d))
		} else {
			num, err := strconv.ParseInt(value, 0, 64)
			if err != nil {
				return err
			}
			field.SetInt(num)
		}

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		num, err := strconv.ParseUint(value, 0, valueType.Bits())
		if err != nil {
			return err
		}
		field.SetUint(num)
	case reflect.Float32, reflect.Float64:
		num, err := strconv.ParseFloat(value, valueType.Bits())
		if err != nil {
			return err
		}
		field.SetFloat(num)
	case reflect.Bool:
		b, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		field.SetBool(b)
	case reflect.String:
		field.SetString(value)
	case reflect.Slice:
		sv, err := parseSlice(valueType, value, sep, layout)
		if err != nil {
			return err
		}
		field.Set(*sv)
	case reflect.Map:
		mv, err := parseMap(valueType, value, sep, layout)
		if err != nil {
			return err
		}
		field.Set(*mv)
	}
	return nil
}
