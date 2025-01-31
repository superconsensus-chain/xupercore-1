package xvm

import (
	"fmt"
	"io/ioutil"
	"os"
	osexec "os/exec"
	"path/filepath"

	"github.com/xuperchain/xvm/runtime/wasi"

	"github.com/xuperchain/xupercore/kernel/contract"
	"github.com/xuperchain/xupercore/kernel/contract/bridge"
	"github.com/xuperchain/xvm/compile"
	"github.com/xuperchain/xvm/exec"
	"github.com/xuperchain/xvm/runtime/emscripten"
	gowasm "github.com/xuperchain/xvm/runtime/go"
)

const (
	currentContractMethodInitialize = "initialize"
)

type xvmCreator struct {
	cm       *codeManager
	config   bridge.InstanceCreatorConfig
	vmconfig *contract.WasmConfig

	wasm2cPath string
}

// 优先查找跟xchain同级目录的二进制，再在PATH里面找
func lookupWasm2c() (string, error) {
	// 首先查找跟xchain同级的目录
	wasm2cPath := filepath.Join(filepath.Dir(os.Args[0]), "wasm2c")
	stat, err := os.Stat(wasm2cPath)
	if err == nil {
		if m := stat.Mode(); !m.IsDir() && m&0111 != 0 {
			return filepath.Abs(wasm2cPath)
		}
	}
	// 再查找系统PATH目录
	return osexec.LookPath("wasm2c")
}

func newXVMCreator(creatorConfig *bridge.InstanceCreatorConfig) (bridge.InstanceCreator, error) {
	wasm2cPath, err := lookupWasm2c()
	if err != nil {
		return nil, err
	}
	creator := &xvmCreator{
		wasm2cPath: wasm2cPath,
		config:     *creatorConfig,
	}
	if creatorConfig.VMConfig != nil {
		creator.vmconfig = creatorConfig.VMConfig.(*contract.WasmConfig)
		optlevel := creator.vmconfig.XVM.OptLevel
		if optlevel < 0 || optlevel > 3 {
			return nil, fmt.Errorf("bad xvm optlevel:%d", optlevel)
		}
	}
	creator.cm, err = newCodeManager(creator.config.Basedir,
		creator.CompileCode, creator.MakeExecCode)
	if err != nil {
		return nil, err
	}
	return creator, nil
}

func cpfile(dest, src string) error {
	buf, err := ioutil.ReadFile(src)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(dest, buf, 0700)
}

func (x *xvmCreator) CompileCode(buf []byte, outputPath string) error {
	tmpdir, err := ioutil.TempDir("", "xvm-compile")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)
	wasmpath := filepath.Join(tmpdir, "code.wasm")
	err = ioutil.WriteFile(wasmpath, buf, 0600)
	if err != nil {
		return err
	}

	libpath := filepath.Join(tmpdir, "code.so")

	cfg := &compile.Config{
		Wasm2cPath: x.wasm2cPath,
		OptLevel:   x.vmconfig.XVM.OptLevel,
	}
	err = compile.CompileNativeLibrary(cfg, libpath, wasmpath)
	if err != nil {
		return err
	}
	return cpfile(outputPath, libpath)
}

func (x *xvmCreator) getContractCodeCache(name string, cp bridge.ContractCodeProvider) (*contractCode, error) {
	return x.cm.GetExecCode(name, cp)
}

func (x *xvmCreator) MakeExecCode(libpath string) (exec.Code, bool, error) {
	resolvers := []exec.Resolver{
		gowasm.NewResolver(),
		emscripten.NewResolver(),
		newSyscallResolver(x.config.SyscallService),
		builtinResolver,
		wasi.NewResolver(),
	}
	//AOT only for experiment;
	// if x.vmconfig.TEEConfig.Enable {
	// TODO: teevm
	// teeResolver, err := teevm.NewTrustFunctionResolver(x.vmconfig.TEEConfig)
	// if err != nil {
	// 	return nil, err
	// }
	// resolvers = append(resolvers, teeResolver)
	// }

	resolver := exec.NewMultiResolver(
		resolvers...,
	)
	// TODO @fengjin
	// newAOTCode shoule accept []byte as arguement rather than string
	code, err := exec.NewAOTCode(libpath, resolver)
	if err != nil {
		return nil, false, err
	}
	legacy, err := isLegacyAOT(libpath)
	if err != nil {
		return nil, false, err
	}
	return code, legacy, err
}

func (x *xvmCreator) CreateInstance(ctx *bridge.Context, cp bridge.ContractCodeProvider) (bridge.Instance, error) {
	code, err := x.getContractCodeCache(ctx.ContractName, cp)
	if err != nil {
		// log.Error("get contract cache error", "error", err, "contract", ctx.ContractName)
		return nil, err
	}

	return createInstance(ctx, code, x.config.SyscallService)
}

func (x *xvmCreator) RemoveCache(contractName string) {
	x.cm.RemoveCode(contractName)
}

func isLegacyAOT(filepath string) (bool, error) {
	syms, err := resolveSymbols(filepath)

	if err != nil {
		return false, err
	}
	if _, ok := syms[currentContractMethodInitialize]; ok {
		return false, nil
	}
	return true, nil

}
func init() {
	bridge.Register(bridge.TypeWasm, "xvm", newXVMCreator)
}
