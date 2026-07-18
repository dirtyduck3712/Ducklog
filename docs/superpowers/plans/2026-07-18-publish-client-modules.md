# 發佈 client / zapsink 模組(可 go get)Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 `client` / `zapsink` 兩個 module 正名為 `github.com/dirtyduck3712/ducklog/{client,zapsink}`,讓外部專案能 `go get`,取代整包複製。

**Architecture:** 純 module path / import / 文件變更,無執行邏輯改動。root `module ducklog` 不動(與兩 lib 完全解耦)。monorepo 內開發續用 `replace => ../client`;發佈走前綴 tag。

**Tech Stack:** Go multi-module monorepo(root go 1.24、client go 1.22、zapsink go 1.24)。

## Global Constraints

- **root `module ducklog` 不動**;只改 `client` + `zapsink`。
- 路徑基底 `github.com/dirtyduck3712/ducklog`(小寫;public repo,小寫 URL 已驗證可解析)。
- zapsink 保留 `replace ... => ../client` 供 repo 內開發;`require` 指真實 tag `v0.1.0`。
- 無程式邏輯變更——不動 client/zapsink 任何 `.go` 的實作,只改 import 路徑字串與 go.mod。
- 設計來源:`docs/superpowers/specs/2026-07-18-publish-client-modules-design.md`。
- 發佈(打 tag / push)是 merge 後與 Ian 協調的步驟,不在 subagent task 內(見文末 Post-merge)。

---

### Task 1: 正名 client / zapsink module path 與 import

**Files:**
- Modify: `client/go.mod:1`
- Modify: `zapsink/go.mod:1,6,12`
- Modify: `zapsink/core.go:11`
- Modify: `zapsink/core_test.go:8`

**Interfaces:**
- Produces: module path `github.com/dirtyduck3712/ducklog/client`、`github.com/dirtyduck3712/ducklog/zapsink`(Task 2 文件引用)。

- [ ] **Step 1: 改 client/go.mod**

`client/go.mod` 第 1 行:
```
module ducklog/client
```
→
```
module github.com/dirtyduck3712/ducklog/client
```
(其餘不變:`go 1.22`。)

- [ ] **Step 2: 改 zapsink/go.mod**

`zapsink/go.mod`:
```
module ducklog/zapsink
...
require (
	ducklog/client v0.0.0
	go.uber.org/zap v1.27.1
)
...
replace ducklog/client => ../client
```
把三處 `ducklog/*` 路徑改為完整路徑、require 版本改 `v0.1.0`:
```
module github.com/dirtyduck3712/ducklog/zapsink
...
require (
	github.com/dirtyduck3712/ducklog/client v0.1.0
	go.uber.org/zap v1.27.1
)
...
replace github.com/dirtyduck3712/ducklog/client => ../client
```
(`go.uber.org/zap`、`multierr` indirect 那行不動。)

- [ ] **Step 3: 改 zapsink import**

`zapsink/core.go:11` 與 `zapsink/core_test.go:8`:
```go
	"ducklog/client"
```
→
```go
	"github.com/dirtyduck3712/ducklog/client"
```
(兩檔各一處;import 群組內其餘行不動。)

- [ ] **Step 4: 驗證兩個 module build/test 綠**

Run（client 與 zapsink 是各自獨立 module,分別在其目錄跑）:
```bash
cd client   && go build ./... && go test ./... ; cd ..
cd zapsink  && go build ./... && go test ./... ; cd ..
```
（zapsink 吃 `replace => ../client` 連本地 client。若 zapsink 測試需 VL,設 `VL_BINARY=/tmp/claude-1000/-home-dva-workspace-docklog/f9022b92-4536-49e5-8e38-c24f296b0368/scratchpad/vl/victoria-logs-prod`;不需要則忽略。）
Expected: 兩 module 皆 build + test PASS。

- [ ] **Step 5: 確認 root 未受影響 + 無殘留裸路徑**

Run:
```bash
go build ./... && go vet ./...
grep -rn '"ducklog/client"\|"ducklog/zapsink"' --include='*.go' . | grep -v phase0 || echo "NO_BARE_IMPORT"
grep -rn 'module ducklog/\|=> ../client' --include='*.mod' .
```
Expected: root 綠;`.go` 無裸 import(印 `NO_BARE_IMPORT`);`.mod` 只剩 `module ducklog`(root,允許)與 `replace github.com/.../ducklog/client => ../client`(zapsink,RHS `../client` 是路徑非模組名,允許)。

- [ ] **Step 6: Commit**

```bash
git add client/go.mod zapsink/go.mod zapsink/core.go zapsink/core_test.go
git commit -m "refactor(modules): client/zapsink 正名為 github.com/dirtyduck3712/ducklog/*

裸 module path 無法 go get;改為 VCS 路徑供外部安裝。
zapsink 保留 replace => ../client 供 repo 內開發。root 不動。

Claude-Session: https://claude.ai/code/session_018usf3iQoLwJuvsjzPQmXDN"
```

---

### Task 2: 更新接入文件

**Files:**
- Modify: `docs/INTEGRATION.md`(§0、import 範例、troubleshooting)
- Modify: `client/README.md`(加 go get / import path 說明)

**Interfaces:**
- Consumes: Task 1 的新 module path。

- [ ] **Step 1: 改寫 INTEGRATION.md §0**

`docs/INTEGRATION.md` 第 10-27 行(整個 §0 到分隔線前)替換為:
```markdown
## 0. 先讓你的專案抓得到 ducklog（模組解析)

ducklog 的 transport 模組已發佈在公開 repo,直接 `go get`:
```
go get github.com/dirtyduck3712/ducklog/client@latest
```
用 zap 的服務再加:
```
go get github.com/dirtyduck3712/ducklog/zapsink@latest
```
import 用完整路徑(見下方各節)。

> 本機開發 ducklog 自身時,zapsink 以 `replace => ../client` 連本地 client;這只影響 repo 內開發,不影響 `go get` 的消費者。
```

- [ ] **Step 2: 改 INTEGRATION.md import 範例**

`docs/INTEGRATION.md:36`:
```go
import "ducklog/client"
```
→
```go
import "github.com/dirtyduck3712/ducklog/client"
```

`docs/INTEGRATION.md:58-59`:
```go
    "ducklog/client"
    "ducklog/zapsink"
```
→
```go
    "github.com/dirtyduck3712/ducklog/client"
    "github.com/dirtyduck3712/ducklog/zapsink"
```

`docs/INTEGRATION.md:54` 句中 `用 ducklog/zapsink 這個轉接橋` → `用 github.com/dirtyduck3712/ducklog/zapsink 這個轉接橋`。

- [ ] **Step 3: 改 INTEGRATION.md troubleshooting**

`docs/INTEGRATION.md:133`:
```
| 別人 build 失敗 `cannot find module ducklog/client` | 本機 replace 只在你機器有效 → 要發佈(見 §0-B) |
```
→
```
| 別人 build 失敗 `cannot find module github.com/dirtyduck3712/ducklog/client` | 用 `go get github.com/dirtyduck3712/ducklog/client@latest`(見 §0),別用本機 replace |
```

- [ ] **Step 4: client/README.md 加安裝說明**

`client/README.md` 在開頭介紹段(第 4 行結尾)之後插入一行:
```markdown

安裝:`go get github.com/dirtyduck3712/ducklog/client@latest`,import path 為 `github.com/dirtyduck3712/ducklog/client`。
```

- [ ] **Step 5: 驗證文件無殘留裸路徑**

Run:
```bash
grep -rn 'ducklog/client\|ducklog/zapsink' --include='*.md' . | grep -v docs/superpowers | grep -v phase0 || echo "DOCS_CLEAN"
```
Expected: 印 `DOCS_CLEAN`(客戶文件已無裸路徑;歷史 specs/plans 保留原樣不算)。若還有命中,逐一核對是否該改。

- [ ] **Step 6: Commit**

```bash
git add docs/INTEGRATION.md client/README.md
git commit -m "docs: 接入改用 go get github.com/dirtyduck3712/ducklog/*

INTEGRATION §0 以 go get 為主要接法,import 範例改完整路徑。

Claude-Session: https://claude.ai/code/session_018usf3iQoLwJuvsjzPQmXDN"
```

---

## Post-merge 發佈（Ian 授權 push;不在 subagent task 內)

merge branch → main、push main 後:

1. 打前綴 tag 並 push（發佈這兩個 module 的 v0.1.0）:
```bash
git tag client/v0.1.0
git tag zapsink/v0.1.0
git push origin client/v0.1.0 zapsink/v0.1.0
```
2. 驗證外部 `go get` 真能動（丟棄式 consumer）:
```bash
d=$(mktemp -d); cd "$d"; go mod init consumer.local/tmp
GOPROXY=direct go get github.com/dirtyduck3712/ducklog/client@v0.1.0
# 寫最小 main.go: import 該 path、呼叫 client.NewRemoteHandler(client.RemoteConfig{...})
go build ./... && echo "GO_GET_OK"
```
（新 tag 若 proxy.golang.org 尚未索引,用 `GOPROXY=direct` 直取。）
3. （選,cosmetic）Ian 在 GitHub 把 repo 從 `Ducklog` rename 成 `ducklog`。
4. （repo 外）更新 `~/.claude/skills/ducklog-integration/SKILL.md`:把「複製」接法改成 `go get`(此檔在 Ian 全域 config,不進本 branch)。

---

## Self-Review

**Spec coverage:**
- spec §1 client/go.mod → Task 1 Step 1 ✓
- spec §2 zapsink/go.mod(module/require/replace)→ Task 1 Step 2 ✓
- spec §3 zapsink import → Task 1 Step 3 ✓
- spec §4 文件(INTEGRATION/client README)→ Task 2 ✓（root README.md 經 grep 確認無 import 引用,故不列入——手術改變）
- spec §5 skill(repo 外)→ Post-merge 4 ✓
- spec §6 發佈/tag → Post-merge 1-2 ✓
- 驗證(repo 內 replace build + 發佈後 go get)→ Task 1 Step 4-5 + Post-merge 2 ✓

**Placeholder scan:** 無 TBD/TODO;每個 code/doc step 附確切前後內容與行號。Post-merge 的 consumer main.go 為「最小呼叫」描述(發佈後現寫),非 plan 內佔位。

**Type consistency:** module path `github.com/dirtyduck3712/ducklog/client`、`.../zapsink` 全 plan 一致;tag 名 `client/v0.1.0`、`zapsink/v0.1.0` 與 require `v0.1.0` 一致。
