// +build darwin

package xvm

import (
	"bytes"
	"debug/macho"
	"io/ioutil"
	"strings"
)

const (
	exportSymbolPrefix = "_export_"
)

func resolveSymbols(filepath string) (map[string]struct{}, error) {
	content, err := ioutil.ReadFile(filepath)
	if err != nil {
		return nil, err
	}
	file, err := macho.NewFile(bytes.NewReader(content))
	if err != nil {
		return nil, err
	}
	symbols := file.Symtab.Syms
	ret := map[string]struct{}{}
	for _, sym := range symbols {
		if strings.HasPrefix(sym.Name, exportSymbolPrefix) {
			ret[sym.Name] = struct{}{}
		}
	}
	return ret, nil
}
