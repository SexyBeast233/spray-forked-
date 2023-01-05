package internal

import (
	"context"
	"fmt"
	"github.com/antonmedv/expr"
	"github.com/antonmedv/expr/vm"
	"github.com/chainreactors/logs"
	"github.com/chainreactors/spray/pkg"
	"github.com/chainreactors/spray/pkg/ihttp"
	"github.com/chainreactors/words"
	"github.com/chainreactors/words/rule"
	"github.com/panjf2000/ants/v2"
	"github.com/valyala/fasthttp"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	max          = 2147483647
	maxRedirect  = 3
	maxCrawl     = 3
	maxRecursion = 0
)

func NewPool(ctx context.Context, config *pkg.Config) (*Pool, error) {
	pctx, cancel := context.WithCancel(ctx)
	pool := &Pool{
		Config:      config,
		ctx:         pctx,
		cancel:      cancel,
		client:      ihttp.NewClient(config.Thread, 2, config.ClientType),
		baselines:   make(map[int]*pkg.Baseline),
		urls:        make(map[string]int),
		tempCh:      make(chan *pkg.Baseline, config.Thread),
		checkCh:     make(chan int),
		additionCh:  make(chan *Unit, 100),
		closeCh:     make(chan struct{}),
		wg:          sync.WaitGroup{},
		initwg:      sync.WaitGroup{},
		reqCount:    1,
		failedCount: 1,
	}

	p, _ := ants.NewPoolWithFunc(config.Thread, func(i interface{}) {
		atomic.AddInt32(&pool.Statistor.ReqTotal, 1)
		unit := i.(*Unit)
		req, err := pool.genReq(unit.path)
		if err != nil {
			logs.Log.Error(err.Error())
			return
		}
		req.SetHeaders(pool.Headers)

		start := time.Now()
		resp, reqerr := pool.client.Do(pctx, req)
		if pool.ClientType == ihttp.FAST {
			defer fasthttp.ReleaseResponse(resp.FastResponse)
			defer fasthttp.ReleaseRequest(req.FastRequest)
		}

		// compare与各种错误处理
		var bl *pkg.Baseline
		if reqerr != nil && reqerr != fasthttp.ErrBodyTooLarge {
			pool.failedCount++
			atomic.AddInt32(&pool.Statistor.FailedNumber, 1)
			bl = &pkg.Baseline{UrlString: pool.BaseURL + unit.path, IsValid: false, ErrString: reqerr.Error(), Reason: ErrRequestFailed.Error()}
			pool.failedBaselines = append(pool.failedBaselines, bl)
		} else {
			if unit.source <= 3 || unit.source == CrawlSource {
				bl = pkg.NewBaseline(req.URI(), req.Host(), resp)
			} else {
				if pool.MatchExpr != nil {
					// 如果非wordsource, 或自定义了match函数, 则所有数据送入tempch中
					bl = pkg.NewBaseline(req.URI(), req.Host(), resp)
				} else if err = pool.PreCompare(resp); err == nil {
					// 通过预对比跳过一些无用数据, 减少性能消耗
					bl = pkg.NewBaseline(req.URI(), req.Host(), resp)
					if err != ErrRedirect && bl.RedirectURL != "" {
						if bl.RedirectURL != "" && !strings.HasPrefix(bl.RedirectURL, "http") {
							bl.RedirectURL = "/" + strings.TrimLeft(bl.RedirectURL, "/")
							bl.RedirectURL = pool.BaseURL + bl.RedirectURL
						}
						pool.wg.Add(1)
						pool.doRedirect(bl, unit.depth)
					}
					pool.addFuzzyBaseline(bl)
				} else {
					bl = pkg.NewInvalidBaseline(req.URI(), req.Host(), resp, err.Error())
				}
			}
		}

		if bl.BodyLength > ihttp.DefaultMaxBodySize {
			bl.ExceedLength = true
		}
		bl.Source = int(unit.source)
		bl.ReqDepth = unit.depth
		bl.Spended = time.Since(start).Milliseconds()
		switch unit.source {
		case InitRandomSource:
			bl.Collect()
			pool.random = bl
			pool.addFuzzyBaseline(bl)
			pool.initwg.Done()
		case InitIndexSource:
			bl.Collect()
			pool.index = bl
			pool.wg.Add(1)
			pool.doCrawl(bl)
			pool.initwg.Done()
		case CheckSource:
			if bl.ErrString != "" {
				logs.Log.Warnf("[check.error] %s maybe ip had banned, break (%d/%d), error: %s", pool.BaseURL, pool.failedCount, pool.BreakThreshold, bl.ErrString)
			} else if i := pool.random.Compare(bl); i < 1 {
				if i == 0 {
					if pool.Fuzzy {
						logs.Log.Warn("[check.fuzzy] maybe trigger risk control, " + bl.String())
					}
				} else {
					pool.failedCount += 2
					logs.Log.Warn("[check.failed] maybe trigger risk control, " + bl.String())
					pool.failedBaselines = append(pool.failedBaselines, bl)
				}
			} else {
				pool.resetFailed() // 如果后续访问正常, 重置错误次数
				logs.Log.Debug("[check.pass] " + bl.String())
			}

		case WordSource:
			// 异步进行性能消耗较大的深度对比
			pool.tempCh <- bl
			pool.reqCount++
			if pool.reqCount%pool.CheckPeriod == 0 {
				pool.reqCount++
				pool.doCheck()
			} else if pool.failedCount%pool.ErrPeriod == 0 {
				pool.failedCount++
				pool.doCheck()
			}
			pool.bar.Done()
		case RedirectSource:
			bl.FrontURL = unit.frontUrl
			pool.tempCh <- bl
		case CrawlSource, ActiveSource, RuleSource, BakSource:
			pool.tempCh <- bl
		}

	})

	pool.reqPool = p
	// 挂起一个异步的处理结果线程, 不干扰主线程的请求并发
	go func() {
		for bl := range pool.tempCh {
			if _, ok := pool.Statistor.Counts[bl.Status]; ok {
				pool.Statistor.Counts[bl.Status]++
			} else {
				pool.Statistor.Counts[bl.Status] = 1
			}

			var params map[string]interface{}
			if pool.MatchExpr != nil || pool.FilterExpr != nil || pool.RecuExpr != nil {
				params = map[string]interface{}{
					"index":   pool.index,
					"random":  pool.random,
					"current": bl,
				}
				for _, status := range FuzzyStatus {
					if bl, ok := pool.baselines[status]; ok {
						params["bl"+strconv.Itoa(status)] = bl
					} else {
						params["bl"+strconv.Itoa(status)] = &pkg.Baseline{}
					}
				}
			}

			var status bool
			if pool.MatchExpr != nil {
				if CompareWithExpr(pool.MatchExpr, params) {
					status = true
				}
			} else {
				if pool.BaseCompare(bl) {
					status = true
				}
			}

			if status {
				pool.Statistor.FoundNumber++
				if pool.FilterExpr != nil && CompareWithExpr(pool.FilterExpr, params) {
					pool.Statistor.FilteredNumber++
					bl.Reason = ErrCustomFilter.Error()
					bl.IsValid = false
				}
			} else {
				bl.IsValid = false
			}

			// 如果要进行递归判断, 要满足 bl有效, mod为path-spray, 当前深度小于最大递归深度
			if bl.IsValid {
				pool.wg.Add(2)
				pool.doCrawl(bl)
				pool.doRule(bl)
				if bl.RecuDepth < maxRecursion {
					if CompareWithExpr(pool.RecuExpr, params) {
						bl.Recu = true
					}
				}
			}
			pool.OutputCh <- bl
			pool.wg.Done()
		}

		pool.analyzeDone = true
	}()
	return pool, nil
}

type Pool struct {
	*pkg.Config
	Statistor       *pkg.Statistor
	client          *ihttp.Client
	reqPool         *ants.PoolWithFunc
	bar             *pkg.Bar
	ctx             context.Context
	cancel          context.CancelFunc
	tempCh          chan *pkg.Baseline // 待处理的baseline
	checkCh         chan int           // 独立的check管道， 防止与redirect/crawl冲突
	additionCh      chan *Unit
	closeCh         chan struct{}
	reqCount        int
	failedCount     int
	isFailed        bool
	failedBaselines []*pkg.Baseline
	random          *pkg.Baseline
	index           *pkg.Baseline
	baselines       map[int]*pkg.Baseline
	urls            map[string]int
	analyzeDone     bool
	worder          *words.Worder
	locker          sync.Mutex
	wg              sync.WaitGroup
	initwg          sync.WaitGroup // 初始化用, 之后改成锁
}

func (pool *Pool) Init() error {
	// 分成两步是为了避免闭包的线程安全问题
	pool.initwg.Add(1)
	pool.reqPool.Invoke(newUnit("/", InitIndexSource))
	pool.initwg.Wait()
	if pool.index.ErrString != "" {
		return fmt.Errorf(pool.index.String())
	}
	logs.Log.Info("[baseline.index] " + pool.index.Format([]string{"status", "length", "spend", "title", "frame", "redirect"}))
	if pool.index.Status == 200 || (pool.index.Status/100) == 3 {
		pool.OutputCh <- pool.index
	}

	pool.initwg.Add(1)
	pool.reqPool.Invoke(newUnit(pkg.RandPath(), InitRandomSource))
	pool.initwg.Wait()
	// 检测基本访问能力
	if pool.random.ErrString != "" {
		return fmt.Errorf(pool.random.String())
	}
	logs.Log.Info("[baseline.random] " + pool.random.Format([]string{"status", "length", "spend", "title", "frame", "redirect"}))

	if pool.random.RedirectURL != "" {
		// 自定协议升级
		// 某些网站http会重定向到https, 如果发现随机目录出现这种情况, 则自定将baseurl升级为https
		rurl, err := url.Parse(pool.random.RedirectURL)
		if err == nil && rurl.Hostname() == pool.random.Url.Hostname() && pool.random.Url.Scheme == "http" && rurl.Scheme == "https" {
			logs.Log.Infof("baseurl %s upgrade http to https", pool.BaseURL)
			pool.BaseURL = strings.Replace(pool.BaseURL, "http", "https", 1)
		}
	}

	return nil
}

func (pool *Pool) checkRedirect(redirectURL string) bool {
	if redirectURL == pool.random.RedirectURL {
		// 相同的RedirectURL将被认为是无效数据
		return false
	} else {
		// path为3xx, 且与baseline中的RedirectURL不同时, 为有效数据
		return true
	}
}

func (pool *Pool) genReq(s string) (*ihttp.Request, error) {
	if pool.Mod == pkg.HostSpray {
		return ihttp.BuildHostRequest(pool.ClientType, pool.BaseURL, s)
	} else if pool.Mod == pkg.PathSpray {
		return ihttp.BuildPathRequest(pool.ClientType, pool.BaseURL, s)
	}
	return nil, fmt.Errorf("unknown mod")
}

func (pool *Pool) Run(ctx context.Context, offset, limit int) {
	pool.worder.RunWithRules()
	if pool.Active {
		pool.wg.Add(1)
		go pool.doActive()
	}

	if pool.Bak {
		pool.wg.Add(1)
		go pool.doBak()
	}
	go func() {
		for {
			pool.wg.Wait()
			pool.closeCh <- struct{}{}
		}
	}()

Loop:
	for {
		select {
		case u, ok := <-pool.worder.C:
			if !ok {
				continue
			}
			pool.Statistor.End++
			if int(pool.reqCount) < offset {
				pool.reqCount++
				continue
			}

			if pool.Statistor.End > limit {
				continue
			}

			if u == "" {
				continue
			}
			pool.wg.Add(1)
			pool.reqPool.Invoke(newUnit(u, WordSource))
		case source := <-pool.checkCh:
			pool.Statistor.CheckNumber++
			if pool.Mod == pkg.HostSpray {
				pool.reqPool.Invoke(newUnit(pkg.RandHost(), source))
			} else if pool.Mod == pkg.PathSpray {
				pool.reqPool.Invoke(newUnit(pkg.RandPath(), source))
			}
		case unit, ok := <-pool.additionCh:
			if !ok {
				continue
			}
			pool.reqPool.Invoke(unit)
		case <-pool.closeCh:
			break Loop
		case <-ctx.Done():
			break Loop
		case <-pool.ctx.Done():
			break Loop
		}
	}

	for pool.analyzeDone {
		time.Sleep(time.Duration(100) * time.Millisecond)
	}
	pool.Statistor.EndTime = time.Now().Unix()
	pool.Close()
}

func (pool *Pool) PreCompare(resp *ihttp.Response) error {
	status := resp.StatusCode()
	if IntsContains(WhiteStatus, status) {
		// 如果为白名单状态码则直接返回
		return nil
	}
	if pool.random != nil && pool.random.Status != 200 && pool.random.Status == status {
		return ErrSameStatus
	}

	if IntsContains(BlackStatus, status) {
		return ErrBadStatus
	}

	if IntsContains(WAFStatus, status) {
		return ErrWaf
	}

	if !pool.checkRedirect(resp.GetHeader("Location")) {
		return ErrRedirect
	}

	return nil
}

func (pool *Pool) BaseCompare(bl *pkg.Baseline) bool {
	if !bl.IsValid {
		return false
	}
	var status = -1
	base, ok := pool.baselines[bl.Status] // 挑选对应状态码的baseline进行compare
	if !ok {
		if pool.random.Status == bl.Status {
			// 当other的状态码与base相同时, 会使用base
			ok = true
			base = pool.random
		} else if pool.index.Status == bl.Status {
			// 当other的状态码与index相同时, 会使用index
			ok = true
			base = pool.index
		}
	}

	if ok {
		if status = base.Compare(bl); status == 1 {
			bl.Reason = ErrCompareFailed.Error()
			return false
		}
	}

	bl.Collect()
	//if !pool.IgnoreWaf {
	//	// 部分情况下waf的特征可能是全局, 指定了--ignore-waf则不会进行waf的指纹检测
	//	for _, f := range bl.Frameworks {
	//		if f.HasTag("waf") {
	//			pool.Statistor.WafedNumber++
	//			bl.Reason = ErrWaf.Error()
	//			return false
	//		}
	//	}
	//}

	if ok && status == 0 && base.FuzzyCompare(bl) {
		pool.Statistor.FuzzyNumber++
		bl.Reason = ErrFuzzyCompareFailed.Error()
		pool.putToFuzzy(bl)
		return false
	}

	return true
}

func CompareWithExpr(exp *vm.Program, params map[string]interface{}) bool {
	res, err := expr.Run(exp, params)
	if err != nil {
		logs.Log.Warn(err.Error())
	}

	if res == true {
		return true
	} else {
		return false
	}
}

func (pool *Pool) doRedirect(bl *pkg.Baseline, depth int) {
	defer pool.wg.Done()
	if depth >= maxRedirect {
		return
	}

	if uu, err := url.Parse(bl.RedirectURL); err == nil && uu.Hostname() == pool.index.Url.Hostname() {
		pool.wg.Add(1)
		pool.additionCh <- &Unit{
			path:     uu.Path,
			source:   RedirectSource,
			frontUrl: bl.UrlString,
			depth:    depth + 1,
		}
	}
}

func (pool *Pool) doCrawl(bl *pkg.Baseline) {
	defer pool.wg.Done()
	if !pool.Crawl {
		return
	}
	bl.CollectURL()
	for _, u := range bl.URLs {
		if strings.HasPrefix(u, "//") {
			u = bl.Url.Scheme + u
		} else if strings.HasPrefix(u, "/") {
			// 绝对目录拼接
			u = pkg.URLJoin(pool.BaseURL, u)
		} else if !strings.HasPrefix(u, "http") {
			// 相对目录拼接
			u = pkg.URLJoin(pool.BaseURL, u)
		}

		if _, ok := pool.urls[u]; ok {
			pool.urls[u]++
		} else {
			// 通过map去重,  只有新的url才会进入到该逻辑
			pool.locker.Lock()
			pool.urls[u] = 1
			pool.locker.Unlock()
			if bl.ReqDepth < maxCrawl {
				parsed, err := url.Parse(u)
				if err != nil {
					continue
				}
				if parsed.Host != bl.Url.Host {
					// 自动限定scoop, 防止爬到其他网站
					continue
				}
				pool.wg.Add(1)
				go pool.addAddition(&Unit{
					path:   parsed.Path,
					source: CrawlSource,
					depth:  bl.ReqDepth + 1,
				})
			}
		}
	}
}

func (pool *Pool) doRule(bl *pkg.Baseline) {
	defer pool.wg.Done()
	if pool.AppendRule == nil {
		return
	}
	if bl.Source == int(RuleSource) || bl.Dir {
		return
	}

	for u := range rule.RunAsStream(pool.AppendRule.Expressions, path.Base(bl.Path)) {
		pool.wg.Add(1)
		go pool.addAddition(&Unit{
			path:   path.Join(path.Dir(bl.Path), u),
			source: RuleSource,
		})
	}
}

func (pool *Pool) doActive() {
	defer pool.wg.Done()
	for _, u := range pkg.ActivePath {
		pool.wg.Add(1)
		pool.addAddition(&Unit{
			path:   safePath(pool.BaseURL, u),
			source: ActiveSource,
		})
	}
}

func (pool *Pool) doBak() {
	defer pool.wg.Done()
	u, err := url.Parse(pool.BaseURL)
	if err != nil {
		return
	}
	worder, err := words.NewWorderWithDsl("{?0}.{@bak_ext}", [][]string{pkg.BakGenerator(u.Host)}, nil)
	if err != nil {
		return
	}
	worder.Run()
	for w := range worder.C {
		pool.wg.Add(1)
		pool.addAddition(&Unit{
			path:   safePath(pool.BaseURL, w),
			source: BakSource,
		})
	}
}

func (pool *Pool) doCheck() {
	if pool.failedCount > pool.BreakThreshold {
		// 当报错次数超过上限是, 结束任务
		pool.recover()
		pool.cancel()
		pool.isFailed = true
		return
	}

	if pool.Mod == pkg.HostSpray {
		pool.checkCh <- CheckSource
	} else if pool.Mod == pkg.PathSpray {
		pool.checkCh <- CheckSource
	}
}

func (pool *Pool) addAddition(u *Unit) {
	pool.additionCh <- u
}

func (pool *Pool) addFuzzyBaseline(bl *pkg.Baseline) {
	if _, ok := pool.baselines[bl.Status]; !ok && IntsContains(FuzzyStatus, bl.Status) {
		bl.Collect()
		pool.locker.Lock()
		pool.wg.Add(1)
		pool.doCrawl(bl)
		pool.baselines[bl.Status] = bl
		pool.locker.Unlock()
		logs.Log.Infof("[baseline.%dinit] %s", bl.Status, bl.Format([]string{"status", "length", "spend", "title", "frame", "redirect"}))
	}
}

func (pool *Pool) putToInvalid(bl *pkg.Baseline, reason string) {
	bl.IsValid = false
	pool.OutputCh <- bl
}

func (pool *Pool) putToFuzzy(bl *pkg.Baseline) {
	bl.IsFuzzy = true
	pool.FuzzyCh <- bl
}

func (pool *Pool) resetFailed() {
	pool.failedCount = 1
	pool.failedBaselines = nil
}

func (pool *Pool) recover() {
	logs.Log.Errorf("%s ,failed request exceeds the threshold , task will exit. Breakpoint %d", pool.BaseURL, pool.reqCount)
	for i, bl := range pool.failedBaselines {
		logs.Log.Errorf("[failed.%d] %s", i, bl.String())
	}
}

func (pool *Pool) Close() {
	for pool.analyzeDone {
		time.Sleep(time.Duration(100) * time.Millisecond)
	}
	close(pool.tempCh)
	close(pool.additionCh)
	pool.bar.Close()
}
