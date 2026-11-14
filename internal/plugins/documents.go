package plugins

import (
	"context"
	"fmt"
	"regexp"
	"unicode/utf8"
	"github.com/ZhenForest/xsbot-core/sdk"
)

var documentID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
var selectorKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func (m *Manager) documentPlugin(id string) (sdk.DocumentEditable, error) {
	p, err := m.managementPlugin(id)
	if err != nil {
		return nil, err
	}
	d, ok := p.(sdk.DocumentEditable)
	if !ok {
		return nil, fmt.Errorf("此插件没有文档编辑能力")
	}
	return d, nil
}

func (m *Manager) DocumentEditors(ctx context.Context, id string) (out []sdk.DocumentEditor, err error) {
	p, err := m.documentPlugin(id)
	if err != nil {
		return nil, err
	}
	err = call(func() error { var e error; out, e = p.DocumentEditors(ctx); return e })
	if err != nil {
		return nil, err
	}
	if len(out) > 16 {
		return nil, fmt.Errorf("文档编辑入口过多")
	}
	seen := map[string]bool{}
	for _, d := range out {
		if !documentID.MatchString(d.ID) || seen[d.ID] || len(d.Label) > 512 || len(d.Hint) > 4096 || len(d.Confirm) > 4096 || (d.Format != "text" && d.Format != "json") || d.MaxBytes < 1 || d.MaxBytes > sdk.MaxDocumentBytes || len(d.Fields) > 16 {
			return nil, fmt.Errorf("插件返回了无效文档编辑描述")
		}
		seen[d.ID] = true
		keys := map[string]bool{}
		for _, f := range d.Fields {
			if !selectorKey.MatchString(f.Key) || keys[f.Key] || f.Key == "constructor" || f.Key == "prototype" || f.Key == "__proto__" || (f.Kind != "text" && f.Kind != "number" && f.Kind != "checkbox") || len(f.Fields) != 0 || len(f.Label) > 512 || len(f.Hint) > 4096 {
				return nil, fmt.Errorf("插件返回了无效文档选择字段")
			}
			keys[f.Key] = true
		}
	}
	return out, nil
}

func (m *Manager) documentEditor(ctx context.Context, id, editor string) (sdk.DocumentEditable, sdk.DocumentEditor, error) {
	entries, err := m.DocumentEditors(ctx, id)
	if err != nil {
		return nil, sdk.DocumentEditor{}, err
	}
	for _, e := range entries {
		if e.ID == editor {
			p, err := m.documentPlugin(id)
			return p, e, err
		}
	}
	return nil, sdk.DocumentEditor{}, fmt.Errorf("未知文档编辑入口")
}

func validDocument(d sdk.Document, max int) bool {
	return documentID.MatchString(d.Key) && documentID.MatchString(d.Revision) && len(d.Content) <= max && utf8.ValidString(d.Content)
}

func (m *Manager) ReadDocument(ctx context.Context, id, editor, input string) (out sdk.Document, err error) {
	if len(input) > sdk.MaxDocumentSelectorBytes {
		return out, fmt.Errorf("文档选择参数过大")
	}
	var object map[string]any
	if e := sdk.DecodeConfiguration([]byte(input), &object); e != nil || object == nil {
		return out, fmt.Errorf("文档选择参数无效")
	}
	p, e, err := m.documentEditor(ctx, id, editor)
	if err != nil {
		return out, err
	}
	err = call(func() error { var err error; out, err = p.ReadDocument(ctx, editor, input); return err })
	if err == nil && !validDocument(out, e.MaxBytes) {
		err = fmt.Errorf("插件返回了无效或超限文档")
	}
	return
}

func (m *Manager) SaveDocument(ctx context.Context, id, editor, key, revision, content string) (out sdk.Document, err error) {
	if !documentID.MatchString(key) || !documentID.MatchString(revision) || len(content) > sdk.MaxDocumentBytes || !utf8.ValidString(content) {
		return out, fmt.Errorf("文档标识、修订或内容无效")
	}
	p, e, err := m.documentEditor(ctx, id, editor)
	if err != nil {
		return out, err
	}
	if len(content) > e.MaxBytes {
		return out, fmt.Errorf("文档内容超过插件限制")
	}
	err = call(func() error {
		var err error
		out, err = p.SaveDocument(ctx, editor, key, revision, content)
		return err
	})
	if err == nil && (!validDocument(out, e.MaxBytes) || out.Key != key) {
		err = fmt.Errorf("插件保存后返回无效结果，请重新读取确认")
	}
	return
}
