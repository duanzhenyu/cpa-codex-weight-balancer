# CPA Codex Weight Balancer

[English](README.md) | **简体中文**

一个 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 插件：让 Codex 凭据的权重跟着各账号剩余的周额度**以及这份额度多快过期**同步变化，使宿主的 `weighted-round-robin` 选择器把新会话分摊到各个账号上，而不是一次抽干一个——且不碰会话粘性。

插件从不参与逐请求的选号。它声明 `management_api` 与 `usage_plugin`，按定时器运行，把 `weight` 字段写回每个凭据。路由完全留在宿主手里，所以 `session-affinity` 照常工作。

`usage_plugin` 订阅宿主的用量记录，记录里带着上游响应头。Codex 在这批头里报告的，正是探测所取的那两个账号级窗口，所以自上一轮以来服务过请求的凭据直接按这份读数定价，省掉一次探测。宿主从自己的队列分发这些记录，因此不在请求路径上，钩子也从不写权重。读数只在定时器上改权重；429 会排队一次探测，这次探测可以立即把该账号排除（见「已知限制」）。

## 为什么看距重置时间，而不只看剩余额度

额度不结转。窗口重置时剩下的部分直接蒸发。所以权重的依据是速率，而不是存量：

```
rate = remaining / max(time_to_reset, t_floor) ^ alpha
```

`alpha = 1` 时，这就是账号要恰好在自己的重置时刻用完周预算所必须维持的消耗速率。重置越近的账号因此拿到越多流量。`alpha = 0` 忽略期限、只按剩余额度加权；大于 1 则更偏向最接近失去额度的那个账号。

**这是启发式，不是最优解。** 当流量填不满整个车队的总容量时，按比例分配仍会浪费额度——最早截止优先（earliest-deadline-first）浪费得更少。这个速率是好的紧迫度信号，但不能证明零蒸发。

## Codex 的两个窗口能做什么、不能做什么

Codex 把每个限流窗口报告为一个单纯的 `used_percent`，外加 `limit_window_seconds` 和重置时间。**载荷里任何地方都没有套餐容量**，所以周百分比和 5 小时百分比是不同的单位，不能合并。

因此：

- **周窗口驱动权重**（`limit_window_seconds` 最大的那个）
- **短窗口只是可用性闸门**——用完即 `weight: 0`，直到它自己重置，随后自动恢复

只有在所有账号都是同一套餐时，百分比才*在同一窗口的账号之间*可比。`quota_capacity` 是全车队共用的单个数值：它把额度与观测消耗换成绝对单位，但不能让不同套餐的账号变得可比——那需要按账号设容量，本版本不建模。它也不改变路由：量化器的锚点随它缩放，所以打开它不会移动任何权重。状态接口报告的 `capacity_scaled` 是这个设置本身，不是对套餐档位的判定。Plus/Pro 混合车队仍不受支持。

## 休眠账号

Codex 的限流窗口从账号的**第一次计费请求**开始起算，不按日程走。在那之前，账号报告的重置时间恰好在一整个窗口之后，并一直这么报：这个值随墙钟平移，而不是倒计时。额度探测点不着它——只有推理请求能。

这样的凭据永远被打分为 `remaining 1.0 / t = 一整个窗口`，是个常数。它永远积累不到本插件赖以工作的期限紧迫度，而每个已起算账号的 `t` 都在向各自的重置时刻缩短。一个已起算但闲置、离重置还有一天的账号，分数大约是休眠账号的七倍；休眠账号则根本到不了那一步。

`kickstart: true` 会在这种凭据上花一个最小推理请求来点着它的时钟，然后交回正常定价。这是本插件里唯一花额度的东西，所以默认关闭，`dry_run` 也会抑制它。宿主标记为不可用的凭据、以及自身额度显示已被限流的凭据同样不发。每个凭据每 `kickstart_cooldown` 至多推一次，按尝试而非成功记账——中途失败的请求可能已在上游计费——且该冷却跨配置重载保留，因为宿主重载配置的频率远高于冷却时长。请求发出前会紧邻重读凭据，因为同一轮可能在这之间已经给它写过权重。

### 5 小时窗口

`kickstart_short_window: true` 把同样的推动用在短预算上，它是独立开关，同样默认关闭。它买到的东西不一样，值得把话说直白：

**它不改变本插件计算出的任何权重。** 分数只来自周窗口；短窗口是纯粹的可用性闸门，只有在*用完*（`remaining <= 0`）时才起作用，而从未起算的窗口是满的。它买到的是上游的东西：时钟已在走的短窗口，相对于之后的一波爆发会更早重置。在开始干活那一刻才起算窗口的账号，打满预算后要从那一刻起被封五小时；时钟提前四小时就点着的账号，一小时后就恢复。这段封锁是 `weight 0`，宿主把它当作硬排除，并连带踢掉该凭据的粘性会话，所以缩短它并非无足轻重。

它的花费也更频繁。周时钟点一次就走一周；短窗口会自己过期，过期后闲置账号又读成未动过。**多频繁未经验证**——只观察到从未用过的窗口保持未起算，没观察过过期的窗口。如果过期的窗口也会同样重新变回未起算，闲置账号每次窗口滚动（受 `kickstart_cooldown` 限制）都要花一个最小请求，量级约每天五次。如果 Codex 而是把窗口直接滚进下一个，这个开关只会在从未用过的账号上触发，而那种情况 `kickstart` 本来就会处理。

一发请求在同一瞬间点着两个窗口（2026-09-12 验证：一个服务过请求的账号读数为 `7917/18000` 与 `594717/604800`，两边都正好走了 10083s），所以同时满足两个开关的凭据仍然只花一个请求——而只打开短窗口开关，也会把从未用过的账号的周时钟一起点着。

两个开关都不搬流量。权重只引导*新*会话，所以一个预热过、却没收到任何冷绑定的凭据，周额度照样会没用完就过期——不点也一样会过期，区别只是点着后它至少有机会争到冷绑定。

## 攒下的重置券

Codex 会给账号发放重置券，每张在发放 30 天后过期。每张的标题都是 "Full reset (Weekly + 5 hr)"，所以兑换一张应当同时恢复两个窗口——这里没有兑换过任何一张来确认。一张券就是一整个窗口的额度，并有自己的期限，所以在 `reset_credits: true` 下它完全按一个窗口定价，并加到周速率上：

```
rate = weekly_remaining / max(time_to_reset, t_floor) ^ alpha
     + Σ full_window   / max(time_to_expiry, t_floor) ^ alpha    （每张未过期的券）
```

没有券的账号定价不变；每张券加上自己那一项，越接近过期的券加得越多。目的是在券仍可兑换时把账号用完。**插件从不兑换券**——那由运维在账号用完后，在 CPA 管理页自行决定。在那之前，用完的窗口仍然回 429，所以券只在排除判定之后才加：券永远不会让一个已用完的账号继续服务，也永远不会把账号压到 `weight: 0`。

`/wham/usage` 只带张数，响应头里完全没有，所以这个开关每轮每个凭据多花一次带 token 的 `GET /wham/rate-limit-reset-credits`，无论该账号本轮是否被探测。只有 `available`、套餐支持、类型为 `codex_rate_limits` 的券才算数；其中任何一张缺少可用的 `expires_at`，整次拉取就算失败。拉取失败时保留上次的列表，而不是读成没有券——那会给账号重新定价。复查用该列表定价、不拉取，且列表跨配置重载保留。

## 配置

```yaml
plugins:
  enabled: true
  configs:
    cpa-codex-weight-balancer:
      enabled: true          # 实例开关；宿主默认关闭
      interval: 15m          # 重新定权周期；最小 1m
      alpha: 1               # 紧迫度指数
      t_floor: 15m           # 距重置时间的下钳
      bucket_q: 0.08         # 一个权重桶的相对宽度
      hysteresis: 0.35       # 权重变动前需超出的桶数
      warmup: 2              # 接管权重前需要的成功探测次数
      freshness: 90m         # 冻结的权重超过此时长即标为 stale
      quota_capacity: 0      # >0 时把额度与消耗换成全车队统一的绝对单位
      kickstart: false       # 花一个请求点着休眠的周窗口
      kickstart_short_window: false  # 同上，针对 5h 窗口；不改变任何权重，会复发
      kickstart_model: gpt-5.6-sol   # Codex 在这里拒绝自家更小的模型
      kickstart_cooldown: 1h # 最小 1m；两个开关共用
      error_penalty: false   # 按账号自身的错误率打折；绝不低于 weight 1
      error_window: 12h      # 错误率回看多久；不得短于 interval
      reset_credits: false   # 把攒下的重置券当作额外窗口定价；每轮每个账号多一次请求
      dry_run: false         # 只计算与报告，不写任何东西
      # endpoint: https://chatgpt.com/backend-api/wham/usage   # 必须是 https
```

宿主还必须真正使用权重：

```yaml
routing:
  strategy: "weighted-round-robin"
  session-affinity: true
```

## 状态

两个路由都经 Management API 鉴权，不是公开资源——逐凭据的状态不该在未鉴权的情况下暴露。

```
GET  /v0/management/cpa-codex-weight-balancer/status
POST /v0/management/cpa-codex-weight-balancer/refresh
```

状态载荷包含 auth index、状态、权重与观测消耗，不包含 token、凭据 JSON 与文件名。

每行的 `weight` 是插件自己的目标值，不一定是磁盘上的值：预热期间它读作 0，而凭据保留它原有的权重。

探测失败会冻结该行的状态与权重，并设置它的 `error`；冻结的权重超过 `freshness` 后 `stale` 变为 true。因此从未定过价的凭据，在预热期间和每次探测都失败时，读起来都是 `pending`、权重 0——靠 `error` 区分二者。

`weights_pending` 统计插件想改动的权重数；`weights_written` 统计真正送达宿主的保存数。插件不声称知道宿主是否重置了它的路由累加器——这它观测不到。两者在保存被拒、以及保存前的紧邻重读发现别的写入者已把目标值写好时会不同。被拒的保存在下一轮重试，而不是记为已完成。

重启会从磁盘重读每个凭据的权重，所以已在目标值的不会被提议。但桶不持久化，所以分数正好落在被旧滞回带稳住的位置的账号，重启后仍可能被合理地重新定价。

`observations_pending` 统计自上一轮以来在服务请求时上报了额度、因而将不经探测直接定价的凭据数。它是区分「宿主从未调到 usage 钩子」与「车队只是闲着」的唯一信号。

`rejections_pending` 统计被 429 排队等待探测的凭据数，`last_recheck` 是上一次在定时器之外探测的时间。各行显示账号当前的状态，所以复查做出的排除会在下一轮之前就显示出来；`weights_pending` 与 `weights_written` 仍只描述上一轮。

`error_penalty` 与 `error_window` 回显这两项设置。开关打开时，每行带 `error_rate`：该凭据窗口内的 `failures / max(requests, 10)`，只计账号自身原因的失败，截至上一个定时周期。

`reset_credits` 回显该开关。打开时，每行带 `reset_credits`（持有的未过期券数）、有券时带 `reset_credit_next_expiry`，以及该凭据上次拉取失败期间带 `reset_credits_error`。券 id 从不显示。

`dormant_credentials` 列出周窗口从未起算的 auth index，`short_dormant_credentials` 列出 5 小时窗口未起算的；`kickstarts_sent` / `last_kickstart_error` 用来区分「没有休眠账号」和「检测到休眠，但每次推动都被拒」。

## 已知限制

- **会话粘性限制了杠杆。** 命中绑定的请求根本到不了加权轮询，所以权重只引导*新*会话。如果少数长会话占了大部分消耗，插件只能微调，拉不走账号上已有的负载。状态接口报告从相邻两次额度快照测得的 `observed_burn_per_hour`，让你看到消耗里实际有多少是可控的。
- **给凭据重新定价会清空宿主累积的路由累加器**，作用于该选择器状态里的每个候选，不只是被改的那个。宿主只比较新旧权重向量里都存在的凭据，而该向量只含正权重，所以把一个降到 0 是把它移出候选集，不算作一次变化。按账号的对数分桶加滞回降低了清空的频率，但防不住：账号上了几十个，大多数周期至少有一个会跨桶。`interval` 不要调小。
- **端点按配置原样信任。** 校验只要求 `https`，别无其他，因为钉死主机名会违背这个字段存在的理由。你填的主机会收到每个账号的 access token，所以这里的笔误要当作凭据泄露对待。
- **探测没有总截止时间。** 宿主的请求 API 不带超时字段，所以一个连上后一直不把响应写完的对端会把该轮挂住，关闭或重新配置都要排在它后面等。
- **`weight: 1` 不是「不接新会话」。** 对上权重 100 的同伴，它仍会拿到约 1% 的冷绑定。它的意思是*最小正份额*。
- **`host.auth.save` 整文档替换，没有 compare-and-swap。** 写入前紧邻重读并保留其他所有键，这收窄了与并发 token 刷新的竞态，但没有消除它。
- **探测一直失败时，用完的短窗口会盲恢复。** 即使没有新快照，`weight: 0` 也会在记录的重置时刻解除，以最低权重重新放行该账号。在探测成功前一直排除，会让它在整个故障期间搁浅；以满权重放行则是瞎猜。下一次成功探测会给它正确定价。
- **探测返回的 HTTP 错误按暂时性处理，从不导致排除。** usage 端点的 429 说明的是探测被限流，不是账号不能承接推理，所以冻结上次的有效权重而不是清零。没有退避：持续失败的端点每 `interval` 重新探测一次。
- **探测端点由 Codex 说了算。** 默认 URL 及其载荷结构在 2026-09-09 对真实账号验证过，但没有任何契约保证它们稳定，所以端点可配置——且必须是 `https`，因为每次探测都带 bearer token。任何一方升级后，都先用 `dry_run: true` 跑一次。
- **响应头读数最多可能比探测旧一个周期。** 账号按它最后服务的那个响应定价，而不是按本轮运行的时刻。这份陈旧是自限的，而非有界的：安静到没有近期读数的凭据，其窗口也几乎没动。闲置凭据照旧探测，被排除的凭据也是：在预算用完前开始的请求，可能在排除之后才报告还有余量。
- **响应头同样由 Codex 说了算。** `X-Codex-Primary/Secondary-*` 是 2026-09-12 从真实流量中抓到的，CPA 原样保留，但它不是有文档的契约。不带可用窗口的响应会被丢弃，该凭据退回探测，所以失效模式是丢掉一项优化，而不是写错权重。
- **kickstart 无法从它收到的响应确认成功。** Codex 在给携带额度头的请求计费之前就填好了这批头，所以回应 kickstart 的 200 仍然报告窗口休眠。因此 usage 钩子收到的*周窗口*休眠读数会被丢弃、改为探测该凭据——周窗口驱动分数。*短窗口*休眠的读数则保留：它给出的周价完全没问题，而且它正面证明了背后那个请求刚把短时钟点着，所以它会触发的那次 kickstart 已经付过钱了。这个判断留到一个背后没有流量的周期再做。
- **`kickstart_short_window` 的复发频率未经验证。** 观察到 Codex 会让*从未用过*的窗口无限期保持未起算；*过期*的窗口是否同样重新变回未起算没有观察到，因为当时车队里每个账号的短窗口都在走。无论哪种情况这个开关都安全——冷却会限住它——但最坏情况是每个闲置账号每次滚动一个最小请求，大约每天五次。
- **`kickstart_short_window` 不改变任何权重。** 分数只读周窗口。短窗口只在用完后才作为可用性闸门，而从未起算的窗口是满的，所以推它不会改变本插件写出的任何东西。它的收益在上游：相对于之后的一波爆发更早重置。
- **推理端点不可配置。** `kickstart_model` 可以配，因为 Codex 在那里拒绝自家更小的模型（`gpt-5.3-codex-spark` 回答 "not supported when using Codex with a ChatGPT account"），但 URL 固定为 CPA 自己发往的地址。指向非默认 Codex base URL 的车队只会得到一次失败的 kickstart，而失败是惰性的。
- **429 触发的是探测，不是排除。** 失败的请求仍然从不被当作额度读数：429 回应的是一个用完的窗口，按它定价会以请求频率写出 `weight: 0`——一种会踢掉已绑定粘性会话的硬排除。取而代之的是，该凭据被排队并在定时器之外立即探测，只有这次探测能排除它。定时器之外只落地进入或离开 `weight: 0` 的变动；给仍在服务的账号重新定价仍要等 ticker。同一凭据的多次拒绝会合并，至多每分钟探测一次；已排除的凭据再吃 429 不会单独触发探测，ticker 照样每轮探测它。此外，定时器会在记录的重置时刻后 30 秒复查排除，探测同意就解除；仍读到用完的，一分钟后重试，因为 Codex 是否真在它报告的那一刻重置并未经验证。探测没有证实的 429——预算还有——不做任何改变，到该账号的路由交给宿主，而宿主的恢复时刻来自 `Retry-After` 或上限 30 分钟的盲指数退避，从不来自同一响应里明摆着的 `Reset-After-Seconds`。探测失败的复查在一分钟后重试一次；一直宕着的探测端点交给 ticker。
- **`error_penalty` 只打折，从不排除。** 打开后，宿主报告的 Codex 凭据上的每个请求都计入该凭据的错误率，但与账号本身无关的失败除外：429（交给复查路径）、客户端挂断（499），以及请求本身的错——400/409/413/422，或 body 里点名 `context_length_exceeded`、`previous_response_not_found` 之类，口径照搬宿主自己的 `IsRequestFault`。图像生成工具调用宿主会报成同一次尝试的两条成功记录，只计一次——除非同一凭据上另一个请求的记录恰好落在这两条之间，那只会稀释错误率而不会抬高它。每个定时周期把本轮计数打上该周期的时间戳，折进滑动窗口（`error_window`：默认 12h，若 `interval` 更长则取 `interval`；不得短于 `interval`），并把错误率设为窗口内求和的 `failures / max(requests, 10)`。一份计数在折入满一个窗口后的第一个定时周期移出。`POST /refresh` 与重载后的首轮按当前错误率定价；只有 ticker 折入。周分数在分桶前乘以 `1 − rate`，所以偶发失败被滞回吸收；全部失败的账号沉到 weight 1，并保留它的粘性会话。被罚的账号吸到的流量更少，证据也就更少：干净请求只能逐渐稀释一波报错，而报错离开窗口时惩罚整段解除。长记忆的代价是写入。按默认参数、每轮 20 个请求模拟，连续四轮全失败后恢复干净流量，回升途中写了 11 次，其中四次在第一小时。闲置账号只在样本到期时动：全失败的一波到期动一次；窗口内请求不足 10 个时，每到期一个样本最多动一次。窗口存在内存里：重载会带过去，但丢掉尚未折入的计数；宿主重启则清空。上游故障会让全车队一起受罚——份额几乎不动，但每个权重都会动。窗口长度与 10 次请求的下限是未经实测的占位值，Codex 实际产生哪些失败也未在实盘观察过。
- **`reset_credits` 有冲刺期。** `alpha = 1` 时，单张券在到期前三天的价是一个全新周窗口的 2.3 倍，前一天 7 倍，前一小时 168 倍，到默认 `t_floor` 时 672 倍。在最后那一天，该账号的桶变动频繁，而每次变动都会清空宿主全车队的路由累加器；该账号也可能打满 5 小时窗口而被排除，连带踢掉粘性会话。周窗口在临近重置时也是同样的形状——每张券就多加一段这样的冲刺期。兑换掉的券最多还会在价格里残留一个周期。`/wham/usage` 里 `applicable_available_count` 的含义未知，所以不使用。

## 要求

- 带插件支持编译的 CLIProxyAPI——在任意管理接口响应里检查 `X-Cpa-Support-Plugin: 1`。
- `routing.strategy: "weighted-round-robin"`。其他策略下宿主从不读权重，本插件什么也做不了。
- 已发布的平台之一：

  | 平台 | 需要 | 发布前由谁加载验证 | 另外跑过 |
  |---|---|---|---|
  | `linux/amd64` | glibc ≥ 2.17 | `tools/loadcheck` | CPA 8.0.27 |
  | `linux/arm64` | glibc ≥ 2.17 | arm64 runner 上的 `tools/loadcheck` | — |
  | `windows/amd64` | — | Windows runner 上的 `tools/loadcheck` | — |
  | `darwin/arm64` | macOS ≥ 12（CPA 自身需要 15） | `tools/loadcheck` | CPA 8.0.27 |
  | `darwin/amd64` | macOS ≥ 12（CPA 自身需要 15） | Rosetta 下与 Intel runner 上的 `tools/loadcheck`，以及 runner 上的 CPA 8.0.27 | Rosetta 下的 CPA 8.0.27 |

  glibc 2.17 是 CPA 自己的 Linux 构建所针对的基线；要在运行 CPA 的容器里测，而不是在 Docker 宿主机上。`tools/loadcheck` 按宿主的方式加载动态库、注册它、跑一轮 dry-run（期间回调宿主的调用落在 Go 里并在那里增长栈），再关闭它；在 Linux 和 macOS 上随后调用 `dlclose`，并检查镜像仍在映射中。这一切做两遍，第二遍在第一遍留下的镜像上，就像重新启用插件时那样——覆盖跨 ABI 的往返，不含宿主的其余部分。
- **`darwin/amd64` 用打过补丁的 Go 工具链构建。** 原版 Go 在那里让每个运行时都把当前 goroutine 放在 TLS 槽位 6（`0x30(GS)`），所以原版构建的 Go 插件与 Go 宿主会互相覆盖：Rosetta 下的 CPA 8.0.27 在第一次增长栈的插件调用时崩溃，本插件和商店里的其他插件都一样；`tools/loadcheck` 在原生 Intel Mac 上同样崩溃。本插件的这个库用 [`scripts/go-tls-slot.patch`](scripts/go-tls-slot.patch) 构建，它把插件的运行时挪到苹果保留的另一个槽位 11（`0x58(GS)`），只要反汇编里还有对槽位 6 的访问，构建就失败。所有这样构建的库共用槽位 11，所以一个 Intel 上的 CPA 只能加载其中一个；商店里的 quota-reset-router、claude-seat-pacer 和 kpatdev-quota-router 也用它。这一点是按机制推出来的，没有实际试过。
- **macOS 上库一直留在内存里，直到 CPA 退出。** CPA 停用插件时会调用 `dlclose`，但 Go 运行时无法卸载。Linux 上 Go 链接器给库打了标记，`dlclose` 不会卸掉它；macOS 上没有任何东西这样做，dyld 会在运行时线程还在跑的时候把镜像卸掉。用 0.7.1 的插件代码时，插件停用后的那一分钟里，CPA 8.0.27 在 Apple Silicon 上一直占满一个核，Rosetta 下更多；以 `tools/loadcheck` 为宿主、在 `dlclose` 后驻留 150 秒时，Rosetta 下三次里有两次退出卡死。0.7.2 起插件在初始化时钉住自己的镜像，停用只让它停止工作，与 Linux、Windows 一致。`tools/loadcheck` 会拒绝 `dlclose` 后被卸掉的库；插件停用后一分钟内 CPA 用掉半个核或更多时，`verify-release` 的 Intel job 会失败。
- Go 在 Darwin 和 Windows 上根本不支持把 `c-shared` Go 库加载进 Go 程序（golang/go#29061、#49457），所以对这些平台，上表就是全部的承诺。

## 安装

从插件商店：管理中心 → 插件 → 安装 `cpa-codex-weight-balancer`。

手动：从 [Releases](https://github.com/duanzhenyu/cpa-codex-weight-balancer/releases) 下载你所在平台的 zip 与 `checksums.txt`，运行 `sha256sum --ignore-missing -c checksums.txt`，然后把动态库（`.so`、`.dll` 或 `.dylib`）解压到你的 `plugins.dir`，可选放在 `<goos>/<goarch>/` 子目录下。插件 ID 是文件名去掉扩展名和可选的 `-v<version>` 后缀，所以 `cpa-codex-weight-balancer.so` 和 `cpa-codex-weight-balancer-v0.7.2.so` 都可以——其他名字都会改掉插件名。

在 Docker 下，把 `plugins.dir` 放在挂载卷上：默认的 `"plugins"` 相对于容器的工作目录，容器重建后就没了。替换 CPA 已加载过的动态库需要重启，停用插件之后也一样：库要到 CPA 退出才释放。

然后启用它，并用 `dry_run: true` 先跑一次不写入的。

## 构建

```sh
make            # 对宿主平台执行 vet、test、build
make release    # 生成 <id>_<version>_<goos>_<goarch>.zip，动态库在 zip 根目录
make dist       # 为上面每个平台生成 release zip，外加 checksums.txt
```

需要 CGO。`make dist` 还需要 GNU userland，以及能运行 `linux/amd64` 镜像的 Docker。它在 Docker 里构建 Linux 和 Windows（`scripts/Dockerfile`；由 zig 钉住 glibc 基线），通过 SSH 在 Apple Silicon Mac 上构建两个 Darwin 库（`MAC_HOST`，默认 `macbook`；那边下载需要代理时设 `MAC_PROXY`），并加载每个它能运行的库，`darwin/amd64` 在 Rosetta 下加载。后者需要用 `scripts/go-tls-slot.patch` 从源码编出的 Go，Mac 上编一次后保留在 `~/Library/Caches/cpa-wb-go-tls`；`make build` 拒绝构建它。把它们上传到草稿 release，再对它运行 `.github/workflows/verify-release.yml`，加载 `linux/arm64` 和 `windows/amd64`，并在原生 Intel Mac 上加载 `darwin/amd64`，在那里同时跑一遍 CPA（`scripts/cpa-load-darwin-amd64.sh`）。

## 许可证

MIT。见 [LICENSE](LICENSE)。
