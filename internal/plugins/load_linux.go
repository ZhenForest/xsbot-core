//go:build linux

package plugins

import (
	"fmt"
	"plugin"
	"github.com/ZhenForest/xsbot-core/sdk"
)

func load(file string) (c Candidate) {
	c.File = file
	defer func() {
		if x := recover(); x != nil {
			c.Problem = fmt.Errorf("plugin load panic: %v", x)
		}
	}()
	p, e := plugin.Open(file)
	if e != nil {
		c.Problem = e
		return
	}
	d, e := p.Lookup("Descriptor")
	if e != nil {
		c.Problem = e
		return
	}
	descriptor, ok := d.(*sdk.Descriptor)
	if ok {
		c.Descriptor = *descriptor
	}
	if !ok || !ValidID(descriptor.ID) || descriptor.API != sdk.APIVersion {
		c.Problem = fmt.Errorf("invalid descriptor or SDK version")
		return
	}
	c.Descriptor = *descriptor
	n, e := p.Lookup("New")
	if e != nil {
		c.Problem = e
		return
	}
	c.New, ok = n.(func() sdk.Plugin)
	if !ok {
		c.Problem = fmt.Errorf("invalid New symbol")
	}
	return
}
