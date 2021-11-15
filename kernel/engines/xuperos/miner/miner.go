package miner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/superconsensus-chain/xupercore/bcs/ledger/xledger/state"

	"github.com/golang/protobuf/proto"

	"github.com/superconsensus-chain/xupercore/bcs/ledger/xledger/tx"
	lpb "github.com/superconsensus-chain/xupercore/bcs/ledger/xledger/xldgpb"
	xctx "github.com/superconsensus-chain/xupercore/kernel/common/xcontext"
	"github.com/superconsensus-chain/xupercore/kernel/engines/xuperos/common"
	"github.com/superconsensus-chain/xupercore/kernel/engines/xuperos/xpb"
	"github.com/superconsensus-chain/xupercore/kernel/network/p2p"
	"github.com/superconsensus-chain/xupercore/lib/logs"
	"github.com/superconsensus-chain/xupercore/lib/metrics"
	"github.com/superconsensus-chain/xupercore/lib/timer"
	"github.com/superconsensus-chain/xupercore/lib/utils"
	"github.com/superconsensus-chain/xupercore/protos"
)

// 负责生产和同步区块
type Miner struct {
	ctx *common.ChainCtx
	log logs.Logger
	// 矿工锁，用来确保矿工出块和同步操作串行进行
	minerMutex sync.Mutex
	// 矿工处理区块的队列
	minerQueue int64
	// 记录同步中任务目标区块高度
	inSyncHeight       int64
	inSyncTargetHeight int64
	// 记录同步中任务目标区块id
	inSyncTargetBlockId []byte
	// 标记是否退出运行
	isExit bool
	// 用户等待退出
	exitWG sync.WaitGroup
}

func NewMiner(ctx *common.ChainCtx) *Miner {
	obj := &Miner{
		ctx: ctx,
		log: ctx.GetLog(),
	}

	return obj
}

// 处理P2P网络中接收到的区块
func (t *Miner) ProcBlock(ctx xctx.XContext, block *lpb.InternalBlock) error {
	if ctx == nil || block == nil {
		return common.ErrParameter
	}

	// 1.检查区块有效性和高度，忽略无效或者比当前同步高度低的区块
	blockSize := int64(proto.Size(block))
	maxBlockSize := t.ctx.State.GetMaxBlockSize()
	if blockSize > maxBlockSize {
		ctx.GetLog().Warn("forbidden proc block because block is too large",
			"blockSize", blockSize, "maxBlockSize", maxBlockSize)
		return common.ErrForbidden.More("block is too large")
	}

	if block.GetHeight() < t.inSyncTargetHeight || bytes.Equal(block.GetBlockid(), t.inSyncTargetBlockId) {
		ctx.GetLog().Trace("forbidden proc block because recv block height lower than in sync height",
			"recvHeight", block.GetHeight(), "recvBlockId", utils.F(block.GetBlockid()),
			"inSyncTargetHeight", t.inSyncTargetHeight, "inSyncTargetBlockId",
			utils.F(t.inSyncTargetBlockId))
		return common.ErrForbidden.More("%s", "recv block height lower than in sync height")
	}

	for id, tx := range block.Transactions {
		if !t.ctx.Ledger.IsValidTx(id, tx, block) {
			ctx.GetLog().Warn("forbidden proc block because invalid tx got from the block",
				"txid", utils.F(tx.Txid), "blockId", utils.F(block.Blockid))
			return common.ErrForbidden.More("%s", "invalid tx got from the block")
		}
	}

	atomic.AddInt64(&t.minerQueue, 1)
	defer atomic.AddInt64(&t.minerQueue, -1)
	if t.minerQueue >= t.ctx.EngCtx.EngCfg.MaxBlockQueueSize {
		ctx.GetLog().Warn("forbidden proc block because miner queue full", "minerQueue", t.minerQueue,
			"recvHeight", block.GetHeight(), "recvBlockId", utils.F(block.GetBlockid()),
			"inSyncTargetHeight", t.inSyncTargetHeight, "inSyncTargetBlockId", utils.F(t.inSyncTargetBlockId))
		return common.ErrForbidden.More("miner queue full")
	}

	ctx.GetLog().Info("recv new block", "inSyncTargetHeight", t.inSyncTargetHeight,
		"inSyncTargetBlockId", utils.F(t.inSyncTargetBlockId),
		"recvHeight", block.GetHeight(), "recvBlockId", utils.F(block.GetBlockid()), "txCount", block.TxCount, "size", proto.Size(block))
	// 尝试同步到该高度，如果小于账本高度会被直接忽略
	return t.trySyncBlock(ctx, block)
}

// 启动矿工，周期检查矿工身份
// 同一时间，矿工状态是唯一的。0:休眠中 1:同步区块中 2:打包区块中
func (t *Miner) Start() {
	// 用于监测退出
	t.exitWG.Add(1)
	defer t.exitWG.Done()

	// 启动矿工循环
	var err error
	isMiner := false
	isSync := false
	ledgerTipId := t.ctx.Ledger.GetMeta().TipBlockid
	ledgerTipHeight := t.ctx.Ledger.GetMeta().TrunkHeight
	stateTipId := t.ctx.State.GetLatestBlockid()
	for !t.IsExit() {
		log, _ := logs.NewLogger("", "miner")
		ctx := &xctx.BaseCtx{
			XLog:  log,
			Timer: timer.NewXTimer(),
		}

		ctx.GetLog().Trace("miner running", "ledgerTipHeight", ledgerTipHeight, "ledgerTipId",
			utils.F(ledgerTipId), "stateTipId", utils.F(stateTipId), "err", err)

		// 1.状态机walk，确保状态机和账本一致
		if !bytes.Equal(ledgerTipId, stateTipId) {
			err = t.ctx.State.Walk(ledgerTipId, false)
		}
		// 2.通过共识检查矿工身份
		if err == nil {
			isMiner, isSync, err = t.ctx.Consensus.CompeteMaster(ledgerTipHeight + 1)
			ctx.Timer.Mark("CompeteMaster")
			ctx.GetLog().Trace("compete master result", "height", ledgerTipHeight+1, "isMiner", isMiner, "isSync", isSync, "err", err)
		}
		//当 isMiner=true isSync=true时，为新的一个周期
		flag := false
	//	term , _ := t.ctx.Consensus.GetConsensusStatus()
		//fmt.Printf("D__打印当前term: %d \n",term.GetCurrentTerm())
		flag , err = t.ReadTermTable(ctx)
		if flag == true{
			t.UpdateCacheTable(ctx)
	//		fmt.Printf("D__打印当前term: %d \n",term.GetCurrentTerm())
		}

		// 3.如需要同步，尝试同步网络最新区块
		if err == nil && isMiner && isSync {
			err = t.trySyncBlock(ctx, nil)
			ctx.Timer.Mark("SyncBlock")
		}
		// 4.如果是矿工，出块
		if err == nil && isMiner {
			beginTime := time.Now()
			err = t.mining(ctx,flag)
			metrics.CallMethodHistogram.WithLabelValues("miner", "Mining").Observe(time.Since(beginTime).Seconds())
		}
		// 4.1 删除解冻区块后的部分数据
		//获取高度
		height := t.ctx.Ledger.GetMeta().TrunkHeight - 1
		t.ClearThawTx(height,ctx)

		// 5.如果出错，休眠3s后重试，防止cpu被打满
		if err != nil && !t.IsExit() {
			ctx.GetLog().Warn("miner run occurred error,sleep 3s try", "err", err)
			time.Sleep(3 * time.Second)
		}
		// 6.更新状态
		if !t.IsExit() {
			err = nil
			ledgerTipId = t.ctx.Ledger.GetMeta().TipBlockid
			ledgerTipHeight = t.ctx.Ledger.GetMeta().TrunkHeight
			stateTipId = t.ctx.State.GetLatestBlockid()
		}
	}

	t.log.Trace("miner exited", "ledgerTipHeight", ledgerTipHeight,
		"ledgerTipId", utils.F(ledgerTipId), "stateTipId", utils.F(stateTipId))
}

//读term表
func (t *Miner)ReadTermTable(ctx xctx.XContext) (bool,error){
	batchWrite := t.ctx.Ledger.ConfirmBatch
	//batchWrite.Reset()
	toTable := "tdpos_term"
	termTable := &protos.TermTable{}
	PbTxBuf, kvErr := t.ctx.Ledger.ConfirmedTable.Get([]byte(toTable))
	term , termerror := t.ctx.Consensus.GetConsensusStatus()
	if termerror != nil {
		return false,nil
	}
	if kvErr == nil {
		parserErr := proto.Unmarshal(PbTxBuf, termTable)
		if parserErr != nil {
			ctx.GetLog().Warn("D__读TermTable表错误")
			return false,parserErr
		}
		//如果trem相等并且NewCycle为false,说明重新记录，直接返回
		if termTable.Trem == term.GetCurrentTerm() && termTable.NewCycle == false{
			return false,nil
		}

		if termTable.Trem != term.GetCurrentTerm() {
			termTable.NewCycle = true
			termTable.Trem = term.GetCurrentTerm()
		} else {
			termTable.NewCycle = false
		}
	}else {
		//ctx.GetLog().Warn("D__节点初始化")
		termTable.NewCycle = false
		termTable.Trem = term.GetCurrentTerm()
	}
	//写表
	pbTxBuf, err := proto.Marshal(termTable)
	if err != nil {
		ctx.GetLog().Warn("DT__解析TermTable失败")
		return false,kvErr
	}
	batchWrite.Put(append([]byte(lpb.ConfirmedTablePrefix), toTable...), pbTxBuf)

	kvErr = batchWrite.Write() //原子写入
	if kvErr != nil {
		ctx.GetLog().Warn("DT__刷trem原子写表错误")
		return false,kvErr
	}
	return termTable.NewCycle,nil
}

//刷新缓存表
func (t *Miner)UpdateCacheTable(ctx xctx.XContext){
	batchWrite := t.ctx.Ledger.ConfirmBatch
	//batchWrite.Reset()
	//获取当前全部候选人，将候选人投票分红信息写入
	toTable := "tdpos_freezes_total_assets"
	freetable := &protos.AllCandidate{}
	PbTxBuf, kvErr := t.ctx.Ledger.ConfirmedTable.Get([]byte(toTable))
	if kvErr == nil {
		parserErr := proto.Unmarshal(PbTxBuf, freetable)
		if parserErr != nil {
			ctx.GetLog().Warn("D__读UtxoMetaExplorer表错误")
			return
		}
	}else {
		return
	}

	/*// 本周期所有候选人缓存表，需要用到这个信息的只有投票奖励分配，所以只用一个自定义的struct而不是重新定义一个proto结构
	// key:address --- value:address
	cacheAllCandidate := struct {
		CaCheAllCandidate map[string]string
	}{}
	cacheAllCandidate.CaCheAllCandidate = make(map[string]string)
	cacheAllCandidate.CaCheAllCandidate = freetable.Candidate
	cacheAllCandidateBytes, _ := json.Marshal(cacheAllCandidate)
	ok := t.ctx.Ledger.ConfirmedTable.Put([]byte("cacheAllCandidate"), cacheAllCandidateBytes)
	if ok != nil {
		ctx.GetLog().Warn("V__新周期刷新候选人缓存表错误")
	}*/

	/*
	 * --- 分红模型 ---
	 * 🔺 分红奖励 = 票数 * 每票奖励 - 债务
	 * 🔺 债务更新：
	 *   ① 投票：
	 *     新投票用户：债务 = (本周期开始时)每票奖励 * 票数
	 *     原先有票数用户：债务 += 票数增量 * (本周期开始时)每票奖励
	 *   ② 撤票：
	 *     自动触发历史分红提现（撤销哪个用户的票就提现哪个用户池，每票奖励为本周期结束时数值）
	 *       如果剩余票数>=0，将用户视为新投票用户，重新计算债务
	 *   ③ 提现：查询：每票奖励以本周期结束为准
	 *     债务 += 提现数量
	 * 🔺 每票奖励更新：
	 *   新出块时：
	 *     每票奖励 += 新块奖励 / 总票数 （总票数为0时每票奖励为0）
	 *
	 * 每个周期开始时统计投票信息（检查票数变动），每次出块根据投票信息增加每票奖励
	 * 为防止有不出块的情况——每次mining生成包含desc的vote交易，ledger中confirmBlock解析再更新每票奖励
	 */

	// 分红数据，包括分红奖励池与用户提现队列（map结构，key到账高度，value具体用户提现数据）
	/*bonusData := &protos.AllBonusData{}
	bonusData.BonusPools = make(map[string]*protos.Pool)
	//bonusData.DiscountQueue = make(map[int64]*protos.BonusReward)

	// 所有分红池
	buf, kvAllPoolsErr := t.ctx.Ledger.ConfirmedTable.Get([]byte("all_bonus_data"))
	if kvAllPoolsErr == nil {
		err := proto.Unmarshal(buf, bonusData)
		if err != nil {
			t.log.Error("V__分红数据反序列化失败", err)
			return
		}
	}*/

	for _ , data := range freetable.Candidate{
		//读用户投票表
		CandidateTable := &protos.CandidateRatio{}
		keytable := "ballot_" + data
		PbTxBuf, kvErr := t.ctx.Ledger.ConfirmedTable.Get([]byte(keytable))
		if(kvErr != nil) {
			ctx.GetLog().Warn("D__刷缓存读取UserBallot异常")
		}
		parserErr := proto.Unmarshal(PbTxBuf, CandidateTable)
		if parserErr != nil  {
			ctx.GetLog().Warn("D__刷缓存CandidateRatio表错误")
		}
		//候选人缓存表
		key := "cache_" + data
		table := &protos.CacheVoteCandidate{}
		PbTxBuf, kvErr = t.ctx.Ledger.ConfirmedTable.Get([]byte(key))
		if kvErr != nil {
			//fmt.Printf("DT__当前用户%s第一次进来\n",key)
		}else {
			parserErr := proto.Unmarshal(PbTxBuf, table)
			if parserErr != nil{
				ctx.GetLog().Warn("DT__读UserReward表错误")
				return
			}
		}
		table.VotingUser = CandidateTable.VotingUser
		table.Ratio = CandidateTable.Ratio
		table.TotalVote = CandidateTable.BeVotedTotal

		// 某候选人/矿工分红池子
		/*pool := &protos.Pool{}
		// 该池子的投票者信息
		pool.Voters = make(map[string]*protos.Voter)
		// 本周期开始时存在历史分红池子
		if kvAllPoolsErr == nil {
			// 本周期的此出块人是否在历史分红池中
			_, ok := bonusData.BonusPools[data]
			if ok {
				// 之前的周期出过块，检查投票者票数变动（新增投票者/追加、撤销投票）
				pool.BonusPerVote = bonusData.BonusPools[data].BonusPerVote
				pool.TotalVotes = table.TotalVote
				// 检查票数变动
				for newestVoter, number := range table.VotingUser {
					// 投票者信息
					voter := &protos.Voter{} // 注意是引用
					oldNumber, ok := bonusData.BonusPools[data].Voters[newestVoter]
					if ok {
						// ok 表示原先投过票，新周期可能不变/增加/减少，票数不变，忽略
						if oldNumber.Amount == number {
							//fmt.Println("新旧周期票数相等")
							voter.Amount = number
							voter.Debt = oldNumber.Debt
							pool.Voters[newestVoter] = voter
							continue
						}
						oldVotes, _ := big.NewInt(0).SetString(oldNumber.Amount, 10)
						newVotes, _ := big.NewInt(0).SetString(number, 10)
						//fmt.Println("新票数", number, "旧票数", oldNumber.Amount)
						// 每票奖励
						bonusPer, _ := big.NewInt(0).SetString(pool.BonusPerVote, 10)
						//fmt.Println("每票奖励", bonusPer, e1)
						// 旧债务
						oldDebt, _ := big.NewInt(0).SetString(oldNumber.Debt, 10)
						//fmt.Println("旧债务", oldDebt.Int64(), e2)
						if newVotes.Cmp(oldVotes) > 0 { // 旧票数 <nil>报错
							// 追加投票
							voter.Amount = number
							delta := bonusPer.Mul(bonusPer, newVotes.Sub(newVotes, oldVotes))
							//fmt.Println("追加投票，债务增量", delta.Int64())
							// 对原先有票数用户：债务 += 票数增量 * (本周期开始时)每票奖励
							voter.Debt = oldDebt.Add(oldDebt, delta).String()
						}else {
							// 撤销投票 触发对本池子的历史分红提现，提现数量：旧票数*每票奖励-旧债务
							discount := oldVotes.Mul(oldVotes, bonusPer).Sub(oldVotes, oldDebt)
							//fmt.Println("撤销投票，自动提现金额", discount)
							// 到账高度
							height := t.ctx.Ledger.GetMeta().TrunkHeight + 3

							if bonusData.DiscountQueue == nil {
								//fmt.Println("V__提现队列为空", bonusData.DiscountQueue)
								bonusData.DiscountQueue = make(map[int64]*protos.BonusRewardDiscount)
							}
							// 用户提现map
							discountQueue := &protos.BonusRewardDiscount{}
							// 用户提现数据（为discountQueue的子字段）
							userDiscount := make(map[string]string)
							// height高度下是否已存在提现数据
							queue, exist := bonusData.DiscountQueue[height]
							if !exist {
								// height高度下没有提现数据，newestVoter用户提现discount数量的分红
								userDiscount[newestVoter] = discount.String()
							}else {
								// height高度下已存在提现数据
								originAmount, repeatOK := queue.UserDiscount[newestVoter]
								if repeatOK {
									// 有同一个用户的多次提现数据时，合并总量
									oldAmount, _ := big.NewInt(0).SetString(originAmount, 10)
									oldAmount.Add(oldAmount, discount)
									// userDiscount先存旧数据
									userDiscount = bonusData.DiscountQueue[height].UserDiscount
									// newestVoter用户提现oldAmount数量的分红
									userDiscount[newestVoter] = oldAmount.String()
								}else {
									// 不同用户提现，userDiscount先存旧数据
									userDiscount = bonusData.DiscountQueue[height].UserDiscount
									// newestVoter用户提现discount数量的分红
									userDiscount[newestVoter] = discount.String()
								}
							}
							discountQueue.UserDiscount = userDiscount
							bonusData.DiscountQueue[height] = discountQueue
							//fmt.Println("V__完整提现队列", bonusData.DiscountQueue)
							if newVotes.Cmp(big.NewInt(0)) >= 0 {
								// 如果剩余票数 >= 0，将用户视为新投票用户，重新计算债务
								voter.Amount = number
								voter.Debt = newVotes.Mul(newVotes, bonusPer).String()
								// 主要是票数为0也需要记录，否则后面有个pool.Voters[newestVoter] = voter会记voter（的amount、debt）数据空，在一些需要计算voter债务等地方会panic
								//fmt.Println("剩余票数>=0，视为新的投票者", voter)
							}else{
								t.log.Error("V__撤销投票后剩余票数<0，错误", "所有数据", bonusData)
							}
						}
					}else {
						// 新增投票者
						voter.Amount = number
						amount, _ := big.NewInt(0).SetString(number, 10)
						bonusPer, _ := big.NewInt(0).SetString(pool.BonusPerVote, 10)
						// 对于新投票者：债务 = (本周期开始时)每票奖励 * 票数
						voter.Debt = amount.Mul(amount, bonusPer).String()
						//fmt.Println("新的投票者", voter)
					}
					pool.Voters[newestVoter] = voter
				}
				// 检查结束记录变动结果
				bonusData.BonusPools[data] = pool
			}else {
				// 本周期开始时，存在历史池子，但是该历史池子没有本节点信息（即本节点第一次成为出块节点），将信息写到历史池中
				pool.BonusPerVote = big.NewInt(0).String()
				for newVoter, number := range table.VotingUser {
					voter := &protos.Voter{}
					voter.Debt = big.NewInt(0).String()
					voter.Amount = number
					pool.Voters[newVoter] = voter
				}
				pool.TotalVotes = table.TotalVote
				//fmt.Println("有历史池子", pool)
				if bonusData.BonusPools == nil {
					bonusData.BonusPools = make(map[string]*protos.Pool)
				}
				bonusData.BonusPools[data] = pool
			}
		}else {
			// 本周期开始时历史池子完全为空
			pool.BonusPerVote = big.NewInt(0).String()
			for newVoter, number := range table.VotingUser {
				voter := &protos.Voter{}
				voter.Debt = big.NewInt(0).String()
				voter.Amount = number
				pool.Voters[newVoter] = voter
			}
			pool.TotalVotes = table.TotalVote
			//fmt.Println("历史池子完全为空", pool)
			bonusData.BonusPools[data] = pool
		}*/
		//写表
		pbTxBuf, err := proto.Marshal(table)
		if err != nil {
			ctx.GetLog().Warn("DT__解析UtxoMetaExplorer失败")
		}
		batchWrite.Put(append([]byte(lpb.ConfirmedTablePrefix), key...), pbTxBuf)
	}
	// 数据更新，包括分红奖励池子与用户提现数据
	//poolsBytes, _ := proto.Marshal(bonusData)
	//fmt.Println("V__周期刷新，pools数据", bonusData)
	//batchWrite.Put(append([]byte(lpb.ConfirmedTablePrefix), []byte("all_bonus_data")...), poolsBytes)
	kvErr = batchWrite.Write() //原子写入
	if kvErr != nil {
		ctx.GetLog().Warn("DT__刷缓存原子写表错误\n")
	}

}

// 停止矿工
func (t *Miner) Stop() {
	t.isExit = true
	t.exitWG.Wait()
}

func (t *Miner) IsExit() bool {
	return t.isExit
}

// 挖矿生产区块
func (t *Miner) mining(ctx xctx.XContext,flag bool) error {
	ctx.GetLog().Debug("mining start.")
	// 1.获取矿工互斥锁，矿工行为完全串行
	t.minerMutex.Lock()
	defer t.minerMutex.Unlock()

	// 2.状态机walk，确保状态机和账本一致
	ledgerTipId := t.ctx.Ledger.GetMeta().TipBlockid
	stateTipId := t.ctx.State.GetLatestBlockid()
	if !bytes.Equal(ledgerTipId, stateTipId) {
		err := t.ctx.State.Walk(ledgerTipId, false)
		if err != nil {
			ctx.GetLog().Warn("mining walk failed", "ledgerTipId", utils.F(ledgerTipId),
				"stateTipId", utils.F(stateTipId))
			return fmt.Errorf("mining walk failed")
		}
		ctx.GetTimer().Mark("walk")
		stateTipId = t.ctx.State.GetLatestBlockid()
	}

	// 3.共识挖矿前处理
	height := t.ctx.Ledger.GetMeta().TrunkHeight + 1
	now := time.Now()
	truncateTarget, extData, err := t.ctx.Consensus.ProcessBeforeMiner(now.UnixNano())
	ctx.GetTimer().Mark("ProcessBeforeMiner")
	if err != nil {
		ctx.GetLog().Warn("consensus process before miner failed", "err", err)
		return fmt.Errorf("consensus process before miner failed")
	}
	ctx.GetLog().Debug("consensus before miner succ", "truncateTarget", truncateTarget, "extData", string(extData))
	if truncateTarget != nil {
		// 裁剪掉账本目标区块，裁掉的交易判断冲突重新回放，裁剪完后继续出块操作
		if err := t.truncateForMiner(ctx, truncateTarget); err != nil {
			return err
		}
		// 重置高度
		height = t.ctx.Ledger.GetMeta().TrunkHeight + 1
	}

	// 4.打包区块
	beginTime := time.Now()
	block, err := t.packBlock(ctx, height, now, extData,flag)
	ctx.GetTimer().Mark("PackBlock")
	metrics.CallMethodHistogram.WithLabelValues("miner", "PackBlock").Observe(time.Since(beginTime).Seconds())
	if err != nil {
		ctx.GetLog().Warn("pack block error", "err", err)
		return err
	}
	ctx.GetLog().Debug("pack block succ", "height", height, "blockId", utils.F(block.GetBlockid()))

	// 5.账本&状态机&共识确认新区块
	err = t.confirmBlockForMiner(ctx, block)
	if err != nil {
		ctx.GetLog().Warn("confirm block for miner failed", "err", err,
			"blockId", utils.F(block.GetBlockid()))
		return err
	}

	// 6.异步广播新生成的区块
	go t.broadcastBlock(ctx, block)

	ctx.GetLog().Info("finish new block generation", "blockId", utils.F(block.GetBlockid()),
		"height", height, "txCount", block.TxCount, "size", proto.Size(block), "costs", ctx.GetTimer().Print())
	return nil
}

// 裁剪掉账本最新的区块
func (t *Miner) truncateForMiner(ctx xctx.XContext, target []byte) error {
	_, err := t.ctx.Ledger.QueryBlockHeader(target)
	if err != nil {
		ctx.GetLog().Warn("truncate failed because query target error", "err", err)
		return err
	}

	// 状态机回滚到目标状态
	err = t.ctx.State.Walk(target, false)
	if err != nil {
		ctx.GetLog().Warn("truncate failed because state walk error", "ledgerTipId", utils.F(t.ctx.Ledger.GetMeta().TipBlockid),
			"walkTargetBlockId", utils.F(target))
		return err
	}

	// 账本裁剪到这个区块
	err = t.ctx.Ledger.Truncate(target)
	if err != nil {
		ctx.GetLog().Warn("truncate failed because ledger truncate error", "err", err)
		return err
	}

	return nil
}

func (t *Miner) packBlock(ctx xctx.XContext, height int64,
	now time.Time, consData []byte,flag bool) (*lpb.InternalBlock, error) {
	// 区块大小限制
	sizeLimit, err := t.ctx.State.MaxTxSizePerBlock()
	if err != nil {
		return nil, err
	}
	ctx.GetLog().Debug("pack block get max size succ", "sizeLimit", sizeLimit)

	// 1.生成timer交易
	autoTx, err := t.getTimerTx(height)
	if err != nil {
		return nil, err
	}
	if len(autoTx.TxOutputsExt) > 0 {
		sizeLimit -= proto.Size(autoTx)
	}

	ctx.GetLog().Debug("pack block get timer tx succ", "auto tx", autoTx)

	// 2.选择本次要打包的tx
	generalTxList, err := t.getUnconfirmedTx(sizeLimit)
	if err != nil {
		return nil, err
	}
	ctx.GetLog().Debug("pack block get general tx succ", "txCount", len(generalTxList))

	// 2.1 查看节点待解冻信息，看其是否有冻结的
	thawTx, err := t.GetThawTx(height,ctx)
	if err != nil {
		ctx.GetLog().Warn("D__解冻出块时查询解冻信息失败\n","err",err)
		//return nil, err
	}

	// 3.获取矿工奖励交易
	var (
		awardTx *lpb.Transaction
		remainAward *big.Int
		err2 error
	)
	//if height < /*200000*/0 {
		awardTx, remainAward, err2 = t.getAwardTx(height, flag)
	/*}else {
		awardTx, remainAward, err2 = t.getAwardTx(height,false)
	}*/
	if err2 != nil {
		return nil, err2
	}
	ctx.GetLog().Debug("pack block get award tx succ", "txid", utils.F(awardTx.GetTxid()))

	txList := make([]*lpb.Transaction, 0)
	txList = append(txList, awardTx)
	if len(autoTx.TxOutputsExt) > 0 {
		txList = append(txList, autoTx)
	}
	if len(generalTxList) > 0 {
		txList = append(txList, generalTxList...)
	}
	if len(thawTx) > 0 {
		txList = append(txList,thawTx...)
	}

	//投票奖励分配
	if remainAward != nil && remainAward.Int64() > 0 && !flag{
		voteTxs, err :=t.GenerateVoteAward(t.ctx.Address.Address,remainAward)
		if err != nil {
			ctx.GetLog().Warn("D__[Vote_Award] fail to generate vote award",  "err", err)
		}
		txList = append(txList, voteTxs...)
	}

	//bonusData := &protos.AllBonusData{}
	//poolsBytes, getE := t.ctx.Ledger.ConfirmedTable.Get([]byte("all_bonus_data"))
	//if getE == nil {
	//	proto.Unmarshal(poolsBytes, bonusData)
	//}
	//if /*height > 1920000 && */remainAward != nil && remainAward.Int64() > 0 {
	//	// 之前因为数据同步问题而带的flag判定已经去掉
	//	// 包括前面计算矿工奖励交易t.getAwardTx(height,false)时也不再传flag，目前flag只用在周期刷新缓存表
	//	cacheAllCandidate := struct {
	//		CacheAllCandidate map[string]string
	//	}{}
	//	// cacheAllCandidate记录了本周期内所有的候选人缓存
	//	// 不在&protos.AllCandidate{}中读取因为AllCandidate的数据会在撤销候选时候立刻更新
	//	cacheAllCandidateBuf, kvEr := t.ctx.Ledger.ConfirmedTable.Get([]byte("cacheAllCandidate"))
	//	if kvEr == nil {
	//		json.Unmarshal(cacheAllCandidateBuf, &cacheAllCandidate)
	//		// 当前矿工在候选人缓存表（以账本数据为准而不是tdpos合约的bucket）且有剩出块奖励中——更新每票奖励并维护分红提现数据，否则只维护分红提现数据
	//		_, ok := cacheAllCandidate.CacheAllCandidate[t.ctx.Address.Address]
	//		if ok {
	//			myPool := bonusData.BonusPools[t.ctx.Address.Address]
	//			oldBonusPer, _ := big.NewInt(0).SetString(myPool.BonusPerVote, 10)
	//			totalVotes, _ := big.NewInt(0).SetString(myPool.TotalVotes, 10)
	//			//fmt.Println(olde, oldBonusPer, totale, totalVotes)
	//			if totalVotes.Int64() != 0 {
	//				// 每次出块更新每票奖励 每票奖励 += 新块奖励 / 票数
	//				remainAward.Div(remainAward, totalVotes)
	//				myPool.BonusPerVote = remainAward.Add(remainAward, oldBonusPer).String()
	//			}else {
	//				// 总票数为0时每票奖励为0
	//				myPool.BonusPerVote = big.NewInt(0).String()
	//			}
	//			//fmt.Println("出块更新", myPool)
	//			bonusData.BonusPools[t.ctx.Address.Address] = myPool
	//		}
	//	}
	//}
	//// 即使出块时每票奖励没有更新，分红数据也需要更新
	//updatePools, _ := proto.Marshal(bonusData)
	//t.ctx.Ledger.ConfirmedTable.Put([]byte("all_bonus_data"), updatePools)
	//// 更新的数据写进交易中，其它节点也拿到数据并写到账本中
	//desc, _ := proto.Marshal(bonusData)
	//voteTx, e := tx.GenerateVoteAwardTx([]byte(t.ctx.Address.Address), "0", desc)
	//if e != nil {
	//	t.log.Warn("V__记录投票奖励交易信息错误", e)
	//	return nil, e
	//}
	//voteTx.Initiator = t.ctx.Address.Address
	//// 将本交易置顶，保证账本先更新此部分数据，再更新提现分红数据
	//tmpSlice := make([]*lpb.Transaction, 0)
	//tmpSlice = append(tmpSlice, voteTx)
	//txList = append(tmpSlice, txList...)

	// 4.打包区块
	consInfo, err := t.convertConsData(consData)
	if err != nil {
		ctx.GetLog().Warn("convert consensus data failed", "err", err, "consData", string(consData))
		return nil, fmt.Errorf("convert consensus data failed")
	}
	block, err := t.ctx.Ledger.FormatMinerBlock(txList, []byte(t.ctx.Address.Address),
		t.ctx.Address.PrivateKey, now.UnixNano(), consInfo.CurTerm, consInfo.CurBlockNum,
		t.ctx.State.GetLatestBlockid(), consInfo.TargetBits, t.ctx.State.GetTotal(),
		consInfo.Justify, nil, height)
	if err != nil {
		ctx.GetLog().Warn("format block error", "err", err)
		return nil, err
	}

	return block, nil
}

func (t *Miner) convertConsData(data []byte) (*state.ConsensusStorage, error) {
	var consInfo state.ConsensusStorage
	if len(data) < 1 {
		return &consInfo, nil
	}

	err := json.Unmarshal(data, &consInfo)
	if err != nil {
		return nil, err
	}

	return &consInfo, nil
}

func (t *Miner) getTimerTx(height int64) (*lpb.Transaction, error) {
	autoTx, err := t.ctx.State.GetTimerTx(height)
	if err != nil {
		t.log.Error("Get timer tx error", "error", err)
		return nil, common.ErrGenerateTimerTxFailed
	}

	return autoTx, nil
}

func (t *Miner) getUnconfirmedTx(sizeLimit int) ([]*lpb.Transaction, error) {
	unconfirmedTxs, err := t.ctx.State.GetUnconfirmedTx(false)
	if err != nil {
		return nil, err
	}

	txList := make([]*lpb.Transaction, 0)
	for _, tx := range unconfirmedTxs {
		size := proto.Size(tx)
		if size > sizeLimit {
			break
		}
		sizeLimit -= size
		txList = append(txList, tx)
	}

	return txList, nil
}

func (t *Miner) getAwardTx(height int64,flag bool) (*lpb.Transaction, *big.Int,error) {
	amount := t.ctx.Ledger.GenesisBlock.CalcAward(height)
	if amount.Cmp(big.NewInt(0)) < 0 {
		return nil, nil,errors.New("amount in transaction can not be negative number")
	}

	//获取奖励比
	block_award := big.NewInt(0)
	remainAward := big.NewInt(0)
	if flag == false {
		remainAward = t.AssignRewards(t.ctx.Address.Address, amount)
	}
	block_award.Sub(amount, remainAward)
	awardTx, err := tx.GenerateAwardTx(t.ctx.Address.Address, block_award.String(), []byte("award"))
	if err != nil {
		return nil, nil, err
	}

	return awardTx, remainAward,nil
}

//构建解冻交易
func (t * Miner)GetThawTx(height int64,ctx xctx.XContext)([]*lpb.Transaction, error) {
	//先获取节点冻结信息
	txs := []*lpb.Transaction{}
	// 提现分红奖励生成
	//allBonusData := &protos.AllBonusData{}
	//allBonusBytes, getErr := t.ctx.Ledger.ConfirmedTable.Get([]byte("all_bonus_data"))
	//if getErr == nil {
	//	pErr := proto.Unmarshal(allBonusBytes, allBonusData)
	//	if pErr == nil {
	//		queue := allBonusData.GetDiscountQueue()
	//		if discount, ok := queue[height]; ok {
	//			for user, amount := range discount.GetUserDiscount() {
	//				// 生成奖励
	//				bonusTx, e := t.ctx.State.DiscountTx(user, t.ctx.Ledger.ConfirmBatch, amount)
	//				if e != nil {
	//					t.log.Error("V__构造提现分红奖励交易失败", e)
	//					txs = append(txs[:0])
	//					goto node
	//				}
	//				//delete(queue[height].UserDiscount, user)
	//				txs = append(txs, bonusTx)
	//			}
	//			// 提现完成，删除此高度下的提现数据
	//			delete(queue, height)
	//			allBonusData.DiscountQueue = queue
	//			if updateBonusBytes, pErr := proto.Marshal(allBonusData); pErr == nil {
	//				putE := t.ctx.Ledger.ConfirmedTable.Put([]byte("all_bonus_data"), updateBonusBytes)
	//				if putE != nil {
	//					t.log.Warn("V__构建解冻交易结束更新数据失败", putE)
	//				}
	//			}
	//		}
	//	}
	//}
	//node:
	keytable := "nodeinfo_" + "tdos_thaw_total_assets"
	PbTxBuf, kvErr := t.ctx.Ledger.ConfirmedTable.Get([]byte(keytable))
	NodeTable := &protos.NodeTable{}
	if kvErr != nil {
		//fmt.Printf("D__节点中不含解冻信息\n")
		return nil, kvErr
	}
	parserErr := proto.Unmarshal(PbTxBuf, NodeTable)
	if parserErr != nil {
		fmt.Printf("D__解析NodeTable错误，错误码： %s \n",parserErr)
		return nil , parserErr
	}
	batch := t.ctx.Ledger.ConfirmBatch
	//batch.Reset()
	value , ok :=  NodeTable.NodeDetails[height]
	if ok {
		for _ , data := range value.NodeDetail{
			Address := data.Address
			//反转转账,只是凭空构建，交易不记录总资产
			tx,error := t.ctx.State.ReverseTx(Address,batch,data.Amount)
			if error != nil {
				ctx.GetLog().Warn("D__反转转账构造交易失败","error",error)
				return nil, error
			}
			txs = append(txs, tx)
		}
	}else {
		return nil , nil
	}

	//fmt.Printf("D__解冻交易拼接成功\n")
	return txs, nil
}

func (t * Miner)ClearThawTx(height int64,ctx xctx.XContext)error{

	keytable := "nodeinfo_" + "tdos_thaw_total_assets"
	PbTxBuf, kvErr := t.ctx.Ledger.ConfirmedTable.Get([]byte(keytable))
	NodeTable := &protos.NodeTable{}
	if(kvErr != nil) {
		//ctx.GetLog().Warn("D__节点中不含解冻信息")
		return nil
	}
	parserErr := proto.Unmarshal(PbTxBuf, NodeTable)
	if parserErr != nil {
		ctx.GetLog().Warn("D__解析NodeTable错误","parserErr",parserErr)
		return parserErr
	}
	batch := t.ctx.Ledger.ConfirmBatch
	//batch.Reset()
	value , ok :=  NodeTable.NodeDetails[height]
	if ok {
		for _ , data := range value.NodeDetail{
			Address := data.Address
			//删除这个用户解冻中的信息
			keytalbe := "amount_" + Address
			//查看用户是否冻结过
			PbTxBuf, kvErr := t.ctx.Ledger.ConfirmedTable.Get([]byte(keytalbe))
			table := &protos.FrozenAssetsTable{}
			if kvErr != nil {
				ctx.GetLog().Warn("D__确认区块时请冻结资产再操作")
				return kvErr
			}else {
				parserErr := proto.Unmarshal(PbTxBuf, table)
				if parserErr != nil {
					ctx.GetLog().Warn("D__确认区块时读FrozenAssetsTable表错误")
					return parserErr
				}
			}
			newTable := &protos.FrozenAssetsTable{
				Total: table.Total,
				FrozenDetail: table.FrozenDetail,
				Timestamp: table.Timestamp,
			}
			//	fmt.Printf("D__打印table: %s \n",table)
			newAmount := big.NewInt(0)
			newAmount.SetString(table.Total, 10)
			for key ,data := range table.ThawDetail{
				//fmt.Printf("D__打印data: %s \n",data)
				if data.Height > height {
					if newTable.ThawDetail == nil {
						newTable.ThawDetail = make(map[string]*protos.FrozenDetails)
					}
					newTable.ThawDetail[key] = data
				}else {
					//总资产减少
					OldAmount := big.NewInt(0)
					OldAmount.SetString(data.Amount, 10)
					//fmt.Printf("D__总资产减少: %s \n",OldAmount.String())
					newAmount.Sub(newAmount,OldAmount)
				}
			}
			newTable.Total = newAmount.String()
			//写表
			pbTxBuf, err := proto.Marshal(newTable)
			if err != nil {
				ctx.GetLog().Warn("D__解冻时解析NodeTable失败")
				return err
			}
			//fmt.Printf("D__解冻成功，打印newTable : %s \n",newTable)
			batch.Put(append([]byte(lpb.ConfirmedTablePrefix), keytalbe...), pbTxBuf)
			//原子写入
			batch.Write()

		}
	}else {
		return  nil
	}
	//删除当前高度的信息
	delete(NodeTable.NodeDetails,height)
	//写表
	pbTxBuf, err := proto.Marshal(NodeTable)
	if err != nil {
		ctx.GetLog().Warn("D__解冻时解析NodeTable失败")
		return err
	}
	batch.Put(append([]byte(lpb.ConfirmedTablePrefix), keytable...), pbTxBuf)
	//原子写入
	writeErr := batch.Write()
	if writeErr != nil {
		ctx.GetLog().Warn("D__解冻交易时原子写入错误","writeErr", writeErr)
		return writeErr
	}
	//fmt.Printf("D__解冻交易拼接成功\n")
	return  nil

}


func (t *Miner) confirmBlockForMiner(ctx xctx.XContext, block *lpb.InternalBlock) error {
	// 需要转化下，为了共识做一些变更（比如pow）
	origBlkId := block.Blockid
	blkAgent := state.NewBlockAgent(block)
	err := t.ctx.Consensus.CalculateBlock(blkAgent)
	ctx.GetTimer().Mark("CalculateBlock")
	if err != nil {
		ctx.GetLog().Warn("consensus calculate block failed", "err", err,
			"blockId", utils.F(block.Blockid))
		return fmt.Errorf("consensus calculate block failed")
	}
	ctx.GetLog().Trace("start confirm block for miner", "originalBlockId", utils.F(origBlkId),
		"newBlockId", utils.F(block.Blockid))

	// 账本确认区块
	confirmStatus := t.ctx.Ledger.ConfirmBlock(block, false)
	ctx.GetTimer().Mark("ConfirmBlock")
	if confirmStatus.Succ {
		if confirmStatus.Orphan {
			ctx.GetLog().Trace("the mined blocked was attached to branch,no need to play",
				"blockId", utils.F(block.Blockid))
			return nil
		}
		ctx.GetLog().Trace("ledger confirm block success", "height", block.Height,
			"blockId", utils.F(block.Blockid))
	} else {
		ctx.GetLog().Warn("ledger confirm block failed", "err", confirmStatus.Error,
			"blockId", utils.F(block.Blockid))
		return errors.New("ledger confirm block error")
	}

	// 状态机确认区块
	err = t.ctx.State.PlayForMiner(block.Blockid)
	ctx.GetTimer().Mark("PlayForMiner")
	if err != nil {
		ctx.GetLog().Warn("state play error ", "error", err, "blockId", utils.F(block.Blockid))
		return err
	}

	// 共识确认区块
	err = t.ctx.Consensus.ProcessConfirmBlock(blkAgent)
	ctx.GetTimer().Mark("ProcessConfirmBlock")
	if err != nil {
		ctx.GetLog().Warn("consensus confirm block error", "err", err,
			"blockId", utils.F(block.Blockid))
		return err
	}

	ctx.GetLog().Trace("confirm block for miner succ", "blockId", utils.F(block.Blockid))
	return nil
}

// 尝试检查同步节点账本到目标区块
// 如果不指定目标区块，则从临近节点查询获取网络状态
func (t *Miner) trySyncBlock(ctx xctx.XContext, targetBlock *lpb.InternalBlock) error {
	// 1.获取到同步目标高度
	var err error
	if targetBlock == nil {
		// 广播查询获取网络最新区块
		targetBlock, err = t.getWholeNetLongestBlock(ctx)
		if err != nil {
			ctx.GetLog().Warn("get whole network longest block failed,sync block exit", "err", err)
			return fmt.Errorf("try sync block get whole network longest block failed")
		}
	}
	ctx.GetLog().Debug("Miner::trySyncBlock", "targetBlockId", utils.F(targetBlock.GetBlockid()), "targetHeight", targetBlock.GetHeight(),
		"inSyncTargetBlockId", utils.F(t.inSyncTargetBlockId), "inSyncTargetHeight", t.inSyncTargetHeight)

	// 2.获取矿工互斥锁，矿工行为完全串行
	t.minerMutex.Lock()
	defer func() {
		if err != nil {
			// 如果同步出错，更新到当前账本主干高度
			t.inSyncTargetHeight = t.ctx.Ledger.GetMeta().GetTrunkHeight()
			t.inSyncTargetBlockId = t.ctx.Ledger.GetMeta().GetTipBlockid()
		}
		// 释放矿工锁
		t.minerMutex.Unlock()
	}()

	// 3.检查同步目标，忽略目标高度小于正在同步高度的任务
	if targetBlock.GetHeight() < t.inSyncTargetHeight ||
		bytes.Equal(targetBlock.GetBlockid(), t.inSyncTargetBlockId) {
		ctx.GetLog().Trace("ignore block because target block height lower than in sync height",
			"targetBlockHeight", targetBlock.GetHeight(), "targetBlockBlockId",
			utils.F(targetBlock.GetBlockid()), "inSyncTargetHeight", t.inSyncTargetHeight,
			"inSyncTargetBlockId", utils.F(t.inSyncTargetBlockId))
		return nil
	}
	// 检查同步目标是否已经在账本中，忽略已经在账本中任务
	if t.ctx.Ledger.ExistBlock(targetBlock.GetBlockid()) {
		ctx.GetLog().Trace("ignore block because target block has in ledger", "targetBlockId",
			utils.F(targetBlock.GetBlockid()))
		return nil
	}

	// 4.更新同步中区块高度
	t.inSyncTargetHeight = targetBlock.GetHeight()
	t.inSyncTargetBlockId = targetBlock.GetBlockid()

	// 4.状态机walk，确保状态机和账本一致
	ledgerTipId := t.ctx.Ledger.GetMeta().GetTipBlockid()
	stateTipId := t.ctx.State.GetLatestBlockid()
	if !bytes.Equal(ledgerTipId, stateTipId) {
		err = t.ctx.State.Walk(ledgerTipId, false)
		if err != nil {
			ctx.GetLog().Warn("try sync block walk failed", "error", err,
				"ledgerTipId", utils.F(ledgerTipId), "stateTipId", utils.F(stateTipId))
			return fmt.Errorf("try sync block walk failed")
		}
	}

	// 5.启动同步区块到目标高度
	err = t.syncBlock(ctx, targetBlock)
	if err != nil {
		ctx.GetLog().Warn("try sync block failed", "err", err, "targetBlock",
			utils.F(targetBlock.GetBlockid()))
		return fmt.Errorf("try sync block failed")
	}

	ctx.GetLog().Trace("try sync block succ", "targetBlock", utils.F(targetBlock.GetBlockid()))
	return nil
}

// 同步本地账本到指定的目标高度
func (t *Miner) syncBlock(ctx xctx.XContext, targetBlock *lpb.InternalBlock) error {
	// 1.判断账本当前高度，忽略小于账本高度或者等于tip block任务
	if targetBlock.GetHeight() < t.ctx.Ledger.GetMeta().GetTrunkHeight() ||
		bytes.Equal(targetBlock.GetBlockid(), t.ctx.Ledger.GetMeta().GetTipBlockid()) {
		return nil
	}

	// 2.从临近节点拉取本地缺失的区块
	// 可优化为并发拉取，可以优化为批处理，方便查看同步进度
	blkIds, err := t.downloadMissBlock(ctx, targetBlock)
	if err != nil {
		ctx.GetLog().Warn("download miss block failed", "err", err)
		return fmt.Errorf("download miss block failed")
	}

	// 4.如果账本发生变更，触发同步账本和状态机
	defer func() {
		ledgerTipId := t.ctx.Ledger.GetMeta().GetTipBlockid()
		stateTipId := t.ctx.State.GetLatestBlockid()
		if !bytes.Equal(ledgerTipId, stateTipId) {
			ledgerTipId := t.ctx.Ledger.GetMeta().TipBlockid
			// Walk相比PalyAndReport代价更高，后面可以优化下
			err := t.ctx.State.Walk(ledgerTipId, false)
			if err != nil {
				ctx.GetLog().Warn("sync block walk failed", "ledgerTipId", utils.F(ledgerTipId),
					"stateTipId", utils.F(stateTipId), "err", err)
				return
			}
		}
	}()

	// 3.将拉取到的区块加入账本
	ctx.GetLog().Debug("batch confirm block", "blockCount", len(blkIds))
	err = t.batchConfirmBlock(ctx, blkIds)
	if err != nil {
		ctx.GetLog().Warn("batch confirm block to ledger failed", "err", err, "blockCount", len(blkIds))
		return fmt.Errorf("batch confirm block to ledger failed")
	}

	return nil
}

// 从临近节点下载区块保存到临时账本（可以优化为并发下载）
func (t *Miner) downloadMissBlock(ctx xctx.XContext,
	targetBlock *lpb.InternalBlock) ([][]byte, error) {
	// 记录下载到的区块id
	blkIds := make([][]byte, 0)

	// 先把targetBlock存入缓存栈
	ledger := t.ctx.Ledger
	err := ledger.SavePendingBlock(targetBlock)
	if err != nil {
		ctx.GetLog().Warn("save pending block error", "blockId", targetBlock.Blockid, "err", err)
		return blkIds, err
	}
	blkIds = append(blkIds, targetBlock.GetBlockid())

	beginBlock := targetBlock
	for !ledger.ExistBlock(beginBlock.PreHash) {
		if len(beginBlock.PreHash) <= 0 || beginBlock.Height == 0 {
			ctx.GetLog().Error("the genesis block is different",
				"genesisBlockId", utils.F(ledger.GetMeta().RootBlockid),
				"syncGenesisBlockId", utils.F(beginBlock.Blockid))
			return nil, common.ErrGenesisBlockDiff
		}

		block, _ := ledger.GetPendingBlock(beginBlock.PreHash)
		if block != nil {
			beginBlock = block
			blkIds = append(blkIds, block.GetBlockid())
			continue
		}

		// 从临近节点下载区块
		block, err := t.getBlock(ctx, beginBlock.PreHash)
		if err != nil {
			ctx.GetLog().Warn("get block error", "err", err)
			return blkIds, err
		}
		// 保存区块到本地栈中
		err = ledger.SavePendingBlock(block)
		if err != nil {
			ctx.GetLog().Warn("save pending block error", "err", err)
			return blkIds, err
		}
		beginBlock = block
		blkIds = append(blkIds, block.GetBlockid())
	}

	return blkIds, nil
}

func (t *Miner) getBlock(ctx xctx.XContext, blockId []byte) (*lpb.InternalBlock, error) {
	input := &xpb.BlockID{
		Bcname:      t.ctx.BCName,
		Blockid:     blockId,
		NeedContent: true,
	}

	opts := []p2p.MessageOption{
		p2p.WithBCName(t.ctx.BCName),
		// p2p.WithLogId(ctx.GetLog().GetLogId()),
	}
	msg := p2p.NewMessage(protos.XuperMessage_GET_BLOCK, input, opts...)
	responses, err := t.ctx.EngCtx.Net.SendMessageWithResponse(ctx, msg)
	if err != nil {
		ctx.GetLog().Warn("confirm block chain status error", "err", err)
		return nil, err
	}

	for _, response := range responses {
		if response.GetHeader().GetErrorType() != protos.XuperMessage_SUCCESS {
			continue
		}

		var block xpb.BlockInfo
		err = p2p.Unmarshal(response, &block)
		if err != nil {
			ctx.GetLog().Warn("unmarshal block error", "err", err)
			continue
		}

		if block.Block == nil {
			ctx.GetLog().Warn("block is nil", "blockId", utils.F(blockId),
				"from", response.GetHeader().GetFrom())
			continue
		}

		ctx.GetLog().Info("download block succ", "height", block.Block.Height,
			"blockId", utils.F(block.Block.Blockid), "msg_log_id", msg.Header.Logid)
		return block.Block, nil
	}

	return nil, errors.New("no response")
}

// 追加区块到账本中
func (t *Miner) batchConfirmBlock(ctx xctx.XContext, blkIds [][]byte) error {
	if len(blkIds) < 1 {
		return nil
	}

	for index := len(blkIds) - 1; index >= 0; index-- {
		timer := timer.NewXTimer()
		block, err := t.ctx.Ledger.GetPendingBlock(blkIds[index])
		if err != nil {
			ctx.GetLog().Warn("ledger get pending block error",
				"blockId", utils.F(blkIds[index]), "err", err)
			return fmt.Errorf("get pending block failed from ledger")
		}
		timer.Mark("GetPendingBlock")

		valid, err := t.ctx.Ledger.VerifyBlock(block, ctx.GetLog().GetLogId())
		if !valid {
			ctx.GetLog().Warn("the verification of block failed.",
				"blockId", utils.F(blkIds[index]))
			return fmt.Errorf("the verification of block failed from ledger")
		}
		blockAgent := state.NewBlockAgent(block)
		timer.Mark("VerifyBlock")
		isMatch, err := t.ctx.Consensus.CheckMinerMatch(ctx, blockAgent)
		if !isMatch {
			ctx.GetLog().Warn("consensus check miner match failed",
				"blockId", utils.F(blkIds[index]), "err", err)
			return errors.New("consensus check miner match failed")
		}
		timer.Mark("CheckMinerMatch")

		status := t.ctx.Ledger.ConfirmBlock(block, false)
		if !status.Succ {
			ctx.GetLog().Warn("ledger confirm block failed",
				"blockId", utils.F(blkIds[index]), "err", status.Error)
			return errors.New("ledger confirm block failed")
		}
		timer.Mark("ConfirmBlock")

		// 状态机确认区块
		if bytes.Equal(t.ctx.State.GetLatestBlockid(), block.PreHash) {
			err = t.ctx.State.PlayAndRepost(block.Blockid, false, false)
			if err != nil {
				ctx.GetLog().Warn("state play error", "error", err, "height", block.Height, "blockId", utils.F(block.Blockid))
				return err
			}
			timer.Mark("PlayAndRepost")
		}

		err = t.ctx.Consensus.ProcessConfirmBlock(blockAgent)
		if err != nil {
			ctx.GetLog().Warn("consensus process confirm block failed",
				"blockId", utils.F(blkIds[index]), "err", err)
			return errors.New("consensus process confirm block failed")
		}

		ctx.GetLog().Info("confirm block finish", "blockId", utils.F(block.Blockid), "height", block.Height, "txCount", block.TxCount, "size", proto.Size(block), "costs", timer.Print())
	}

	ctx.GetLog().Trace("batch confirm block to ledger succ", "blockCount", len(blkIds))
	return nil
}

// syncConfirm 向周围节点询问块是否可以被接受
func (t *Miner) isConfirmed(ctx xctx.XContext, bcs *xpb.ChainStatus) bool {
	input := &lpb.InternalBlock{Blockid: bcs.Block.Blockid}
	opts := []p2p.MessageOption{
		p2p.WithBCName(t.ctx.BCName),
		p2p.WithLogId(ctx.GetLog().GetLogId()),
	}
	msg := p2p.NewMessage(protos.XuperMessage_CONFIRM_BLOCKCHAINSTATUS, input, opts...)
	response, err := t.ctx.EngCtx.Net.SendMessageWithResponse(t.ctx, msg)
	if err != nil {
		ctx.GetLog().Warn("confirm block chain status error", "err", err)
		return false
	}

	return countConfirmBlock(response)
}

// countConfirmBlockRes 对p2p网络返回的确认区块的结果进行统计
// 统计邻近节点的返回信息
func countConfirmBlock(messages []*protos.XuperMessage) bool {
	agreeCnt := 0
	disagreeCnt := 0
	for _, msg := range messages {
		var bts xpb.TipStatus
		err := p2p.Unmarshal(msg, &bts)
		if err != nil {
			continue
		}

		if bts.GetIsTrunkTip() {
			agreeCnt++
		} else {
			disagreeCnt++
		}
	}

	// 支持的节点需要大于反对的节点，并且支持的节点个数需要大于res的1/3
	return agreeCnt >= disagreeCnt && agreeCnt >= len(messages)/3
}

// 广播新区块
// 三种块传播模式：
//  1. 一种是完全块广播模式(Full_BroadCast_Mode)，即直接广播原始块给所有相邻节点，
//     适用于出块矿工在知道周围节点都不具备该块的情况下；
//  2. 一种是问询式块广播模式(Interactive_BroadCast_Mode)，即先广播新块的头部给相邻节点，
//     相邻节点在没有相同块的情况下通过GetBlock主动获取块数据。
//  3. Mixed_BroadCast_Mode是指出块节点将新块用Full_BroadCast_Mode模式广播，
//     其他节点使用Interactive_BroadCast_Mode
// broadcast block in Full_BroadCast_Mode since it's the original miner
func (t *Miner) broadcastBlock(ctx xctx.XContext, block *lpb.InternalBlock) {
	engCtx := t.ctx.EngCtx
	opts := []p2p.MessageOption{
		p2p.WithBCName(t.ctx.BCName),
		p2p.WithLogId(ctx.GetLog().GetLogId()),
	}
	var msg *protos.XuperMessage
	if engCtx.EngCfg.BlockBroadcastMode == common.InteractiveBroadCastMode {
		blockID := &lpb.InternalBlock{
			Blockid: block.Blockid,
		}
		msg = p2p.NewMessage(protos.XuperMessage_NEW_BLOCKID, blockID, opts...)
	} else {
		msg = p2p.NewMessage(protos.XuperMessage_SENDBLOCK, block, opts...)
	}

	err := engCtx.Net.SendMessage(t.ctx, msg)
	if err != nil {
		ctx.GetLog().Warn("broadcast block error", "p2pLogId", msg.GetHeader().GetLogid(),
			"height", block.Height, "blockId", utils.F(block.GetBlockid()), "err", err)
		return
	}

	ctx.GetLog().Trace("broadcast block succ", "p2pLogId", msg.GetHeader().GetLogid(),
		"blockId", utils.F(block.GetBlockid()))
	return
}

func (t *Miner) getWholeNetLongestBlock(ctx xctx.XContext) (*lpb.InternalBlock, error) {
	opt := []p2p.MessageOption{
		p2p.WithBCName(t.ctx.BCName),
		p2p.WithLogId(ctx.GetLog().GetLogId()),
	}
	msg := p2p.NewMessage(protos.XuperMessage_GET_BLOCKCHAINSTATUS, nil, opt...)
	responses, err := t.ctx.EngCtx.Net.SendMessageWithResponse(t.ctx, msg)
	if err != nil {
		ctx.GetLog().Warn("get block chain status error", "err", err)
		return nil, err
	}

	bcStatus := make([]*xpb.ChainStatus, 0, len(responses))
	for _, response := range responses {
		var status xpb.ChainStatus
		err = p2p.Unmarshal(response, &status)
		if err != nil {
			ctx.GetLog().Warn("unmarshal block chain status error", "err", err)
			continue
		}

		bcStatus = append(bcStatus, &status)
	}

	sort.Sort(BCSByHeight(bcStatus))
	for _, bcs := range bcStatus {
		if t.isConfirmed(ctx, bcs) {
			return bcs.Block, nil
		}
	}

	return nil, errors.New("not found longest block")
}

type BCSByHeight []*xpb.ChainStatus

func (s BCSByHeight) Len() int {
	return len(s)
}
func (s BCSByHeight) Less(i, j int) bool {
	return s[i].LedgerMeta.TrunkHeight > s[j].LedgerMeta.TrunkHeight
}
func (s BCSByHeight) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}
