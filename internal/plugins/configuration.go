package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ZhenForest/xsbot-core/sdk"
)

type ConfigurationPlugin struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Version          string `json:"version"`
	State            string `json:"state"`
	Configurable     bool   `json:"configurable"`
	DocumentEditable bool   `json:"document_editable"`
}

func (m *Manager) ConfigurationPlugins() []ConfigurationPlugin {
	out := []ConfigurationPlugin{}
	for _, i := range m.Instances {
		_, configurable := i.plugin.(sdk.Configurable)
		_, documents := i.plugin.(sdk.DocumentEditable)
		out = append(out, ConfigurationPlugin{i.Descriptor.ID, i.Descriptor.Name, i.Descriptor.Version, i.State(), configurable, documents})
	}
	return out
}

func (m *Manager) managementPlugin(id string) (sdk.Plugin, error) {
	for _, i := range m.Instances {
		if i.Descriptor.ID != id {
			continue
		}
		if !i.initialized.Load() || i.failed.Load() {
			return nil, fmt.Errorf("插件尚未就绪")
		}
		if i.closing.Load() {
			return nil, fmt.Errorf("插件正在关闭")
		}
		return i.plugin, nil
	}
	return nil, fmt.Errorf("插件本次运行不可用；请检查启用状态与优先级")
}

func (m *Manager) configurationPlugin(id string) (sdk.Configurable, error) {
	p, err := m.managementPlugin(id)
	if err != nil {
		return nil, err
	}
	c, ok := p.(sdk.Configurable)
	if !ok {
		return nil, fmt.Errorf("此插件没有配置项")
	}
	return c, nil
}

func (m *Manager) Configuration(ctx context.Context, id string) (out sdk.Configuration, err error) {
	p, err := m.configurationPlugin(id)
	if err != nil {
		return out, err
	}
	err = call(func() error { var e error; out, e = p.Configuration(ctx); return e })
	if err == nil {
		err = redactConfiguration(&out)
	}
	return
}

// A correctly implemented plugin already returns blank passwords; enforce it
// again at the Core boundary before serializing a management response.
func redactConfiguration(c *sdk.Configuration) error {
	if len(c.Values) > 1<<20 {
		return fmt.Errorf("插件配置超过 1 MiB")
	}
	var values map[string]any
	if e := sdk.DecodeConfiguration([]byte(c.Values), &values); e != nil || values == nil {
		return fmt.Errorf("插件返回了无效配置")
	}
	var walk func([]sdk.ConfigurationField, map[string]any, int) error
	walk = func(fields []sdk.ConfigurationField, object map[string]any, depth int) error {
		if depth > 8 {
			return fmt.Errorf("插件配置字段嵌套过深")
		}
		for _, f := range fields {
			if f.Kind == "password" {
				object[f.Key] = ""
			}
			if f.Kind == "list" {
				rows, _ := object[f.Key].([]any)
				for _, row := range rows {
					item, ok := row.(map[string]any)
					if !ok {
						return fmt.Errorf("插件列表配置无效")
					}
					if e := walk(f.Fields, item, depth+1); e != nil {
						return e
					}
				}
			}
		}
		return nil
	}
	if e := walk(c.Fields, values, 0); e != nil {
		return e
	}
	b, e := json.Marshal(values)
	c.Values = string(b)
	return e
}

func (m *Manager) SaveConfiguration(ctx context.Context, id, revision, values string) error {
	if len(values) > 1<<20 {
		return fmt.Errorf("插件配置超过 1 MiB")
	}
	p, err := m.configurationPlugin(id)
	if err != nil {
		return err
	}
	return call(func() error { return p.SaveConfiguration(ctx, revision, values) })
}

func (m *Manager) ConfigurationAction(ctx context.Context, id, revision, action, input string) (out string, err error) {
	if len(input) > 1<<20 {
		return "", fmt.Errorf("插件操作参数超过 1 MiB")
	}
	p, err := m.configurationPlugin(id)
	if err != nil {
		return "", err
	}
	a, ok := p.(sdk.ConfigurationActions)
	if !ok {
		return "", fmt.Errorf("此插件没有管理操作")
	}
	err = call(func() error { var e error; out, e = a.RunConfigurationAction(ctx, revision, action, input); return e })
	return
}
