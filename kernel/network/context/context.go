// 明确定义该模块需要的上下文信息，方便代码阅读和理解
package context

import (
	"context"
	"fmt"

	"github.com/xuperchain/xupercore/kernel/common/xcontext"
	"github.com/xuperchain/xupercore/kernel/network/config"
	"github.com/xuperchain/xupercore/lib/logs"
	"github.com/xuperchain/xupercore/lib/timer"
)

// 考虑到有些对象是有状态的，需要单例实现
// 有些上下文是领域级别的，有些是操作级别的
// 所以对领域级别的上下文和操作级别的上下文分别定义

// 领域级上下文
type DomainCtx interface {
	context.Context

	GetLog() logs.Logger
	GetP2PConf() *config.Config
	GetMetricSwitch() bool
	SetMetricSwitch(s bool)
	IsValid() bool
}

type DomainCtxImpl struct {
	xcontext.BaseCtx
	P2PConf      *config.Config
	MetricSwitch bool
}

// 必须设置的在参数直接指定，可选的通过对应的Set方法设置
func CreateDomainCtx(xlog logs.Logger, confPath string) (DomainCtx, error) {
	if xlog == nil || confPath == "" {
		return nil, fmt.Errorf("create domain context failed because some param are missing")
	}

	// 加载配置
	cfg, err := config.LoadP2PConf(confPath)
	if err != nil {
		return nil, fmt.Errorf("create object context failed because config load fail.err:%v", err)
	}

	ctx := new(DomainCtxImpl)
	ctx.XLog = xlog
	ctx.P2PConf = cfg
	// 可选参数设置默认值
	ctx.MetricSwitch = false

	return ctx, nil
}

func (t *DomainCtxImpl) GetLog() logs.Logger {
	return t.XLog
}

func (t *DomainCtxImpl) GetP2PConf() *config.Config {
	return t.P2PConf
}

func (t *DomainCtxImpl) GetMetricSwitch() bool {
	return t.MetricSwitch
}

func (t *DomainCtxImpl) SetMetricSwitch(s bool) {
	t.MetricSwitch = s
}

func (t *DomainCtxImpl) IsValid() bool {
	if t.XLog == nil || t.P2PConf == nil {
		return false
	}

	return true
}

// 操作级上下文，不是所有的方法都需要独立上下文，按需选用
type OperateCtx interface {
	context.Context

	GetLog() logs.Logger
	GetTimer() *timer.XTimer
	IsValid() bool
}

type OperateCtxImpl struct {
	xcontext.BaseCtx
	// 便于记录各阶段处理耗时
	Timer *timer.XTimer
}

func CreateOperateCtx(xlog logs.Logger, tmr *timer.XTimer) (OperateCtx, error) {
	if xlog == nil || tmr == nil {
		return nil, fmt.Errorf("create operate context failed because some param are missing")
	}

	ctx := new(OperateCtxImpl)
	ctx.XLog = xlog
	ctx.Timer = tmr

	return ctx, nil
}

func (t *OperateCtxImpl) GetLog() logs.Logger {
	return t.XLog
}

func (t *OperateCtxImpl) GetTimer() *timer.XTimer {
	return t.Timer
}

func (t *OperateCtxImpl) IsValid() bool {
	if t.XLog == nil || t.Timer == nil {
		return false
	}

	return true
}

// 其他特殊需要上下文定义
