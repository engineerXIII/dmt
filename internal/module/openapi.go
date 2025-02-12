package module

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"dario.cat/mergo"
	"github.com/go-openapi/spec"
	"github.com/mohae/deepcopy"
	"helm.sh/helm/v3/pkg/chartutil"
	"k8s.io/utils/ptr"

	"github.com/deckhouse/dmt/internal/valuesvalidation"
)

const (
	ExamplesKey = "x-examples"
	ArrayObject = "array"
	ObjectKey   = "object"
)

const (
	imageDigestfile string = "images_digests.json"
)

func applyDigests(digests, values map[string]any) {
	obj := map[string]any{
		"global": map[string]any{
			"modulesImages": map[string]any{
				"digests": digests,
			},
		},
	}

	_ = mergo.Merge(&values, obj, mergo.WithOverride)
}

func helmFormatModuleImages(m *Module, rawValues map[string]any) (chartutil.Values, error) {
	caps := chartutil.DefaultCapabilities
	vers := []string(caps.APIVersions)
	vers = append(vers, "autoscaling.k8s.io/v1/VerticalPodAutoscaler")
	caps.APIVersions = vers

	digests, err := GetModulesImagesDigests(m.GetPath())
	if err != nil {
		return nil, err
	}

	applyDigests(digests, rawValues)
	top := map[string]any{
		"Chart":        m.GetMetadata(),
		"Capabilities": caps,
		"Release": map[string]any{
			"Name":      m.GetName(),
			"Namespace": m.GetNamespace(),
			"IsUpgrade": true,
			"IsInstall": true,
			"Revision":  0,
			"Service":   "Helm",
		},
		"Values": rawValues,
	}

	return top, nil
}

func GetModulesImagesDigests(modulePath string) (modulesDigests map[string]any, err error) {
	var (
		search bool
	)

	if fi, errs := os.Stat(filepath.Join(filepath.Dir(modulePath), imageDigestfile)); errs != nil || fi.Size() == 0 {
		search = true
	}

	if search {
		return DefaultImagesDigests, nil
	}

	modulesDigests, err = getModulesImagesDigestsFromLocalPath(modulePath)
	if err != nil {
		return nil, err
	}

	return modulesDigests, nil
}

func getModulesImagesDigestsFromLocalPath(modulePath string) (map[string]any, error) {
	var digests map[string]any

	imageDigestsRaw, err := os.ReadFile(filepath.Join(filepath.Dir(modulePath), imageDigestfile))
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(imageDigestsRaw, &digests)
	if err != nil {
		return nil, err
	}

	return digests, nil
}

func ComposeValuesFromSchemas(m *Module) (chartutil.Values, error) {
	valueValidator, err := valuesvalidation.NewValuesValidator(m.GetName(), m.GetPath())
	if err != nil {
		return nil, fmt.Errorf("schemas load: %w", err)
	}

	if valueValidator == nil {
		return nil, nil
	}

	camelizedModuleName := ToLowerCamel(m.GetName())

	schema, ok := valueValidator.ModuleSchemaStorages[m.GetName()]
	if !ok || schema.Schemas == nil {
		return nil, nil
	}

	values, ok := valueValidator.ModuleSchemaStorages[m.GetName()].Schemas["values"]
	if values == nil || !ok {
		return nil, fmt.Errorf("cannot find openapi values schema for module %s", m.GetName())
	}

	moduleSchema := *values
	moduleSchema.Default = make(map[string]any)

	values, ok = valueValidator.GlobalSchemaStorage.Schemas["values"]
	var globalSchema spec.Schema
	if ok && values != nil {
		globalSchema = *values
	}
	globalSchema.Default = make(map[string]any)

	combinedSchema := spec.Schema{}
	combinedSchema.Properties = map[string]spec.Schema{camelizedModuleName: moduleSchema, "global": globalSchema}

	rawValues, err := NewOpenAPIValuesGenerator(&combinedSchema).Do()
	if err != nil {
		return nil, fmt.Errorf("generate values: %w", err)
	}

	return helmFormatModuleImages(m, rawValues)
}

type OpenAPIValuesGenerator struct {
	rootSchema *spec.Schema
}

func NewOpenAPIValuesGenerator(schema *spec.Schema) *OpenAPIValuesGenerator {
	return &OpenAPIValuesGenerator{
		rootSchema: schema,
	}
}

func (g *OpenAPIValuesGenerator) Do() (map[string]any, error) {
	return parseProperties(g.rootSchema)
}

func parseProperties(tempNode *spec.Schema) (map[string]any, error) {
	if tempNode == nil {
		return nil, nil
	}

	result := make(map[string]any)
	for key := range tempNode.Properties {
		if err := parseProperty(key, ptr.To(tempNode.Properties[key]), result); err != nil {
			return nil, err
		}
	}

	return result, nil
}

func parseProperty(key string, prop *spec.Schema, result map[string]any) error {
	switch {
	case prop.Extensions[ExamplesKey] != nil:
		return parseExamples(key, prop, result)
	case len(prop.Enum) > 0:
		parseEnum(key, prop, result)
	case prop.Type.Contains(ObjectKey):
		return parseObject(key, prop, result)
	case prop.Default != nil:
		result[key] = prop.Default
	case prop.Type.Contains(ArrayObject) && prop.Items != nil && prop.Items.Schema != nil:
		return parseArray(key, prop, result)
	case prop.AllOf != nil:
		// not implemented
	case prop.OneOf != nil:
		return parseOneOf(key, prop, result)
	case prop.AnyOf != nil:
		return parseAnyOf(key, prop, result)
	}

	return nil
}

func parseExamples(key string, prop *spec.Schema, result map[string]any) error {
	var example any

	switch conv := prop.Extensions[ExamplesKey].(type) {
	case []any:
		example = conv[0]
	case map[string]any:
		example = conv
	}

	if example != nil {
		if prop.Type.Contains(ObjectKey) {
			t, err := parseProperties(prop)
			if err != nil {
				return err
			}
			if err := mergo.Merge(&t, example, mergo.WithOverride); err != nil {
				return err
			}
			result[key] = t

			return nil
		}

		result[key] = example
	}

	return nil
}

func parseEnum(key string, prop *spec.Schema, result map[string]any) {
	t := prop.Enum[0]
	if prop.Default != nil {
		t = prop.Default
	}
	result[key] = t
}

func parseObject(key string, prop *spec.Schema, result map[string]any) error {
	t, err := parseProperties(prop)
	if err != nil {
		return err
	}
	result[key] = t

	return nil
}

func parseArray(key string, prop *spec.Schema, result map[string]any) error {
	if prop.Items.Schema.Default != nil {
		result[key] = prop.Items.Schema.Default

		return nil
	}

	if t, err := parseProperties(prop.Items.Schema); err != nil {
		return err
	} else if t != nil {
		result[key] = t
	}

	return nil
}

func parseOneOf(key string, prop *spec.Schema, result map[string]any) error {
	downwardSchema := deepcopy.Copy(prop).(*spec.Schema)
	mergedSchema := mergeSchemas(downwardSchema, prop.OneOf...)

	t, err := parseProperties(mergedSchema)
	if err != nil {
		return err
	}
	result[key] = t

	return nil
}

func parseAnyOf(key string, prop *spec.Schema, result map[string]any) error {
	downwardSchema := deepcopy.Copy(prop).(*spec.Schema)
	mergedSchema := mergeSchemas(downwardSchema, prop.AnyOf...)

	t, err := parseProperties(mergedSchema)
	if err != nil {
		return err
	}
	result[key] = t

	return nil
}

func mergeSchemas(rootSchema *spec.Schema, schemas ...spec.Schema) *spec.Schema {
	if rootSchema == nil {
		rootSchema = &spec.Schema{}
	}

	if rootSchema.Properties == nil {
		rootSchema.Properties = make(map[string]spec.Schema)
	}

	rootSchema.OneOf = nil
	rootSchema.AllOf = nil
	rootSchema.AnyOf = nil

	for i := range schemas {
		schema := schemas[i]
		for key := range schema.Properties {
			rootSchema.Properties[key] = schema.Properties[key]
		}
		rootSchema.OneOf = schema.OneOf
		rootSchema.AllOf = schema.AllOf
		rootSchema.AnyOf = schema.AnyOf
	}

	return rootSchema
}
