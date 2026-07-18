# VL Basic Auth Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 VictoriaLogs 用 HTTP Basic Auth 關起來(單一共享密鑰),寫入端與讀取端都帶憑證,未設憑證時維持現行無 auth 行為。

**Architecture:** VL 以 `-httpAuth.username/password` 開 auth。寫入端 `client` 的 `RemoteConfig` 移除無效的 `APIKey`(Bearer)、改帶 `Username/Password` 走 `req.SetBasicAuth`。讀取端 `vl.Client` 新增 `NewWithAuth` 建構子(不動既有 `New`,避免波及 13 個呼叫點),`cmd/ducklog-mcp` 讀 `VL_USERNAME/VL_PASSWORD` env。

**Tech Stack:** Go 1.24,純 stdlib。整合測試用 `internal/vltest` 起真 VL。

## Global Constraints

- 純 stdlib,不新增依賴。
- **off-by-default**:VL 未設 `-httpAuth.*` / client user 空 / MCP 無 `VL_USERNAME` 時,三端維持無 auth 行為。
- 憑證非空才帶 `Authorization` header;空則完全不帶。
- 讀取端憑證走**獨立 env** `VL_USERNAME`/`VL_PASSWORD`,不塞進 `VL_URL` userinfo。
- 整合測試需 VL binary(`VL_BINARY` env),`-short` 會 skip。VL binary:`/tmp/claude-1000/-home-dva-workspace-docklog/f9022b92-4536-49e5-8e38-c24f296b0368/scratchpad/vl/victoria-logs-prod`。
- caveat(記文件):Basic Auth 在純 HTTP 上是 base64、非加密;受信任網段使用,需加密再開 VL `-tls`。
- 設計來源:`docs/superpowers/specs/2026-07-18-vl-basic-auth-design.md`。

---

### Task 1: 寫入端 client Basic Auth

**Files:**
- Modify: `client/config.go`(`RemoteConfig`:移除 `APIKey`,加 `Username`/`Password`)
- Modify: `client/sender.go:131`(`post()`:Bearer → Basic)
- Modify: `client/sender_test.go:47-51`(`cfgFor` 移除 `APIKey: "k"`)
- Modify: `client/README.md:17-25`(示範移除 APIKey)
- Test: `client/sender_test.go`(新增兩個 auth 測試)

**Interfaces:**
- Produces: `RemoteConfig{ ..., Username string, Password string }`(不再有 `APIKey`)。Task 3 文件會引用這兩個欄位。

- [ ] **Step 1: 寫失敗測試**

在 `client/sender_test.go` 末尾加(檔案已 import `net/http`、`net/http/httptest`、`sync`、`io`、`testing`、`time`):

```go
func TestSenderSendsBasicAuthWhenConfigured(t *testing.T) {
	var mu sync.Mutex
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cfg := cfgFor(srv.URL)
	cfg.Username = "u"
	cfg.Password = "p"
	s := newSender(cfg, func(time.Duration) {}, time.Now)
	s.start()
	s.enqueue(entry{Service: "api", Level: "info", Message: "x"})
	s.close()

	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Basic dTpw" { // base64("u:p")
		t.Fatalf("Authorization = %q; want %q", gotAuth, "Basic dTpw")
	}
}

func TestSenderNoAuthHeaderWhenUnset(t *testing.T) {
	var mu sync.Mutex
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := newSender(cfgFor(srv.URL), func(time.Duration) {}, time.Now) // 無憑證
	s.start()
	s.enqueue(entry{Service: "api", Level: "info", Message: "x"})
	s.close()

	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "" {
		t.Fatalf("未設憑證不應帶 Authorization,got %q", gotAuth)
	}
}
```

- [ ] **Step 2: 跑測試確認失敗**

Run: `go test ./client/ -run 'TestSenderSendsBasicAuth|TestSenderNoAuthHeader'`
Expected: 編譯錯 `cfg.Username undefined`(欄位還沒加)。

- [ ] **Step 3: 改 config.go**

`client/config.go` 中 `RemoteConfig`:把
```go
	Endpoint      string        // 例:...
	APIKey        string        // Bearer(ingest scope)
	Service       string        // 本服務名稱
```
改成
```go
	Endpoint string // 例:...
	// Username/Password:VL 開了 -httpAuth.* 時的 Basic Auth 憑證;
	// 留空(VL 無 auth)則不帶 Authorization header。
	Username string
	Password string
	Service  string // 本服務名稱
```
並把型別上方 doc comment 的「Endpoint/APIKey/Service 通常必填」改為「Endpoint/Service 通常必填」;`Endpoint` 欄位註解裡「VL OSS 無 auth,APIKey 留空即可」那句刪除。

- [ ] **Step 4: 改 sender.go**

`client/sender.go` 的 `post()` 中,把
```go
	req.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)
```
換成
```go
	if s.cfg.Username != "" {
		req.SetBasicAuth(s.cfg.Username, s.cfg.Password)
	}
```

- [ ] **Step 5: 修既有測試 cfgFor**

`client/sender_test.go` 的 `cfgFor` 移除 `APIKey: "k"`:
```go
func cfgFor(url string) RemoteConfig {
	return RemoteConfig{Endpoint: url, Service: "api",
		BatchSize: 2, FlushInterval: 10 * time.Millisecond, QueueSize: 100,
		Fallback: io.Discard}.withDefaults()
}
```

- [ ] **Step 6: 跑測試確認通過**

Run: `go test ./client/`
Expected: PASS(含新 auth 測試與既有全部)。

- [ ] **Step 7: 改 client/README.md**

`client/README.md` 的程式碼示範(約 17-25 行)把
```go
h := client.NewRemoteHandler(client.RemoteConfig{
    Endpoint: "http://<vl-host>:9428/insert/jsonline?_time_field=ts&_msg_field=message&_stream_fields=service",
    APIKey:   "", // VL OSS 無 auth
    Service:  "my-service",
})
```
改成
```go
h := client.NewRemoteHandler(client.RemoteConfig{
    Endpoint: "http://<vl-host>:9428/insert/jsonline?_time_field=ts&_msg_field=message&_stream_fields=service",
    Service:  "my-service",
    // VL 開了 -httpAuth.* 時填 Basic Auth 憑證;VL 無 auth 則留空:
    // Username: "ingester", Password: "...",
})
```

- [ ] **Step 8: Commit**

```bash
go build ./... && go test ./client/
git add client/config.go client/sender.go client/sender_test.go client/README.md
git commit -m "feat(client): VL Basic Auth,移除對 VL 無效的 Bearer

RemoteConfig 移除 APIKey、改 Username/Password 走 SetBasicAuth;
憑證空則不帶 auth header(off-by-default)。

Claude-Session: https://claude.ai/code/session_018usf3iQoLwJuvsjzPQmXDN"
```

---

### Task 2: 讀取端 vl.Client Basic Auth + MCP 接線 + 整合測試

**Files:**
- Modify: `internal/vl/client.go`(`Client` 加 `user`/`pass`;加 `NewWithAuth`;`Ping`/`Query` 帶 auth)
- Modify: `cmd/ducklog-mcp/main.go`(讀 `VL_USERNAME`/`VL_PASSWORD`)
- Modify: `internal/vltest/vltest.go`(加 `StartVLWithAuth`;health poll 帶 auth)
- Test: `internal/vl/client_test.go`(新增端到端 auth 測試)

**Interfaces:**
- Consumes: `vltest.StartVL`(既有,簽章不變)。
- Produces:
  - `vl.NewWithAuth(baseURL string, timeout time.Duration, user, pass string) *vl.Client`
  - `vltest.StartVLWithAuth(t testing.TB, user, pass string) string`
  - 既有 `vl.New(baseURL, timeout)` 與 `vltest.StartVL(t)` 簽章**不變**(13 個 `vl.New` 呼叫點、8 個 `StartVL` 呼叫點不受影響)。

- [ ] **Step 1: 寫失敗測試**

先讀 `internal/vl/client_test.go` 確認 package 名與既有 import,依其風格在末尾加(需 import `context`、`time`、`ducklog/internal/vl`、`ducklog/internal/vltest`,缺的補上):

```go
func TestBasicAuthGuardsQueries(t *testing.T) {
	if testing.Short() {
		t.Skip("需 VL")
	}
	base := vltest.StartVLWithAuth(t, "reader", "secret")

	// 無憑證 → Ping 應失敗(401)。
	noauth := vl.New(base, 5*time.Second)
	if err := noauth.Ping(context.Background()); err == nil {
		t.Fatal("無憑證的 Ping 應失敗")
	}

	// 對的憑證 → Ping 成功、Query 不報錯(空結果可接受)。
	ok := vl.NewWithAuth(base, 5*time.Second, "reader", "secret")
	if err := ok.Ping(context.Background()); err != nil {
		t.Fatalf("帶對憑證的 Ping 應成功: %v", err)
	}
	if _, err := ok.Query(context.Background(), "* _time:1h", 10); err != nil {
		t.Fatalf("帶對憑證的 Query 應成功: %v", err)
	}
}
```

- [ ] **Step 2: 跑測試確認失敗**

Run: `go test ./internal/vl/ -run TestBasicAuthGuardsQueries`
Expected: 編譯錯(`vltest.StartVLWithAuth`、`vl.NewWithAuth` undefined)。

- [ ] **Step 3: 改 vltest.go 加 StartVLWithAuth**

`internal/vltest/vltest.go`:把現有 `StartVL` 的 body 抽成內部 `startVL(t, user, pass)`,對外保留 `StartVL` 並加 `StartVLWithAuth`。

`StartVL` 改為:
```go
// StartVL 啟動無 auth 的臨時 VictoriaLogs,回傳 base URL。
func StartVL(t testing.TB) string { return startVL(t, "", "") }

// StartVLWithAuth 啟動開了 HTTP Basic Auth 的臨時 VictoriaLogs。
func StartVLWithAuth(t testing.TB, user, pass string) string { return startVL(t, user, pass) }
```

新增 `startVL`,內容同原 `StartVL`,但兩處改動:(a) cmd args 在 `user != ""` 時附 `-httpAuth.*`;(b) health poll 在 `user != ""` 時帶 basic auth。

cmd 組裝改為:
```go
	args := []string{
		"-storageDataPath=" + t.TempDir(),
		"-httpListenAddr=127.0.0.1:" + strconv.Itoa(port),
		"-retentionPeriod=30d",
	}
	if user != "" {
		args = append(args, "-httpAuth.username="+user, "-httpAuth.password="+pass)
	}
	cmd := exec.Command(bin, args...)
```

health poll loop 改為:
```go
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, base+"/health", nil)
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return base
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("VL /health 未在時限內就緒 (%s)", base)
	return ""
```

（`Ingest`/`countAll` 不動——auth 測試不 ingest,只驗證 auth 護欄。）

- [ ] **Step 4: 改 vl/client.go**

`internal/vl/client.go`:

`Client` 加欄位:
```go
type Client struct {
	base string
	hc   *http.Client
	user string
	pass string
}
```

加 `NewWithAuth` 與 `auth` helper(放在 `New` 之後):
```go
// NewWithAuth 同 New,但每個請求附 HTTP Basic Auth(VL 開了 -httpAuth.* 時用)。
func NewWithAuth(baseURL string, timeout time.Duration, user, pass string) *Client {
	c := New(baseURL, timeout)
	c.user, c.pass = user, pass
	return c
}

// auth 在有設憑證時為 request 附上 Basic Auth。
func (c *Client) auth(req *http.Request) {
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
}
```

`Ping`:在 `req, _ := http.NewRequestWithContext(...)` 之後、`c.hc.Do(req)` 之前加 `c.auth(req)`。
`Query`:在 `req, _ := http.NewRequestWithContext(...)` 之後、`c.hc.Do(req)` 之前加 `c.auth(req)`。
(`Count` 走 `Query`,自動涵蓋,不需另改。)

- [ ] **Step 5: 改 main.go**

`cmd/ducklog-mcp/main.go` 的 `main()`:
```go
	vlURL := resolveVLURL()
	c := vl.New(vlURL, 30*time.Second)
	if u := os.Getenv("VL_USERNAME"); u != "" {
		c = vl.NewWithAuth(vlURL, 30*time.Second, u, os.Getenv("VL_PASSWORD"))
	}
	if err := c.Ping(context.Background()); err != nil {
```
(其餘不變;`os` 已 import。)

- [ ] **Step 6: 跑測試確認通過**

Run: `VL_BINARY=/tmp/claude-1000/-home-dva-workspace-docklog/f9022b92-4536-49e5-8e38-c24f296b0368/scratchpad/vl/victoria-logs-prod go test ./internal/vl/ -run TestBasicAuthGuardsQueries -v`
Expected: PASS(真的起帶 auth 的 VL、無憑證被拒、帶憑證成功)。

- [ ] **Step 7: 全量驗證**

Run: `go build ./... && go vet ./... && VL_BINARY=/tmp/claude-1000/-home-dva-workspace-docklog/f9022b92-4536-49e5-8e38-c24f296b0368/scratchpad/vl/victoria-logs-prod go test ./...`
Expected: 全綠(既有 13 個 `vl.New`、8 個 `StartVL` 呼叫點因簽章不變,不受影響)。

- [ ] **Step 8: Commit**

```bash
git add internal/vl/client.go cmd/ducklog-mcp/main.go internal/vltest/vltest.go internal/vl/client_test.go
git commit -m "feat(vl): 讀取端 Basic Auth(NewWithAuth + VL_USERNAME/PASSWORD)

vl.Client 新增 NewWithAuth,Ping/Query 帶 Basic Auth;MCP 讀 env。
vltest 加 StartVLWithAuth 供端到端驗證。New 簽章不變。

Claude-Session: https://claude.ai/code/session_018usf3iQoLwJuvsjzPQmXDN"
```

---

### Task 3: 文件與 run script

**Files:**
- Modify: `scripts/run-dev.sh`(`cmd_vl` 支援選用 auth env;`cmd_mcp` 文件加 `VL_USERNAME/VL_PASSWORD`)
- Modify: `README.md`(env 表加憑證;安全段更新;auth 執行示範 + caveat)

**Interfaces:**
- Consumes: Task 1 的 `Username/Password`、Task 2 的 `VL_USERNAME/VL_PASSWORD` env 與 VL `-httpAuth.*`。

- [ ] **Step 1: run-dev.sh cmd_vl 支援 auth**

`scripts/run-dev.sh`:頂部用法註解的「環境變數」區塊加兩行說明:
```
#   VL_HTTP_USER 若設,VL 以 Basic Auth 啟動(需搭 VL_HTTP_PASS)
#   VL_HTTP_PASS Basic Auth 密碼
```
`cmd_vl` 的 `exec "$bin" ...` 改成先組 auth 參數:
```bash
cmd_vl() {
  local bin
  bin="$(resolve_vl_binary)"
  mkdir -p "$VL_DATA"
  echo "啟動 VictoriaLogs: $bin" >&2
  echo "vmui:  http://$VL_ADDR/select/vmui" >&2
  echo "資料:  $VL_DATA（retention 30d）" >&2
  local auth=()
  if [[ -n "${VL_HTTP_USER:-}" ]]; then
    auth=(-httpAuth.username="$VL_HTTP_USER" -httpAuth.password="${VL_HTTP_PASS:-}")
    echo "auth:  Basic Auth 已開(user=$VL_HTTP_USER)" >&2
  fi
  exec "$bin" \
    -storageDataPath="$VL_DATA" \
    -retentionPeriod=30d \
    -httpListenAddr="$VL_ADDR" \
    "${auth[@]}"
}
```

- [ ] **Step 2: run-dev.sh cmd_mcp 文件帶憑證**

`cmd_mcp` 印出的 `claude mcp add` 示範,在 `--env VL_URL=...` 後補一句說明(不改指令主體,加註解行即可),例如在 heredoc 內 `claude mcp add` 那行之後加:
```
  （VL 有開 auth 時再加 --env VL_USERNAME=<u> --env VL_PASSWORD=<p>）
```

- [ ] **Step 3: README env 表與安全段**

`README.md`:
- env 表(約 89 行 `| VL_URL | ... |` 附近)加兩列:
```
| `VL_USERNAME` | （空） | VL 開 Basic Auth 時的使用者;空則不帶 auth |
| `VL_PASSWORD` | （空） | VL Basic Auth 密碼 |
```
- 安全段(約 112 行「**無 auth**: 靠受信任的網路...」)改寫為:
```
- **Basic Auth（選用）**: VL 以 `-httpAuth.username/-httpAuth.password` 開一組共享 Basic Auth（涵蓋 ingest 與 query）。寫入端設 `RemoteConfig.Username/Password`,MCP 端設 `VL_USERNAME/VL_PASSWORD`。三端留空則維持無 auth。
  未開 auth 時仍請把 VL 綁 localhost / 內網,勿對外曝露。
- **caveat**: Basic Auth 在純 HTTP 上是 base64、非加密;請在受信任網段使用,需端到端加密再開 VL `-tls`。
```
- VL 執行示範區塊(約 40 行 `victoria-logs-prod -storageDataPath=...`)下方加一句開 auth 的示範:
```
開 Basic Auth（選用）:

    victoria-logs-prod -storageDataPath=./data/victoria-logs \
      -retentionPeriod=30d -httpListenAddr=127.0.0.1:9428 \
      -httpAuth.username=admin -httpAuth.password=secret
```

- [ ] **Step 4: 驗證 script 語法**

Run: `bash -n scripts/run-dev.sh && VL_HTTP_USER=admin VL_HTTP_PASS=secret VL_BINARY=/tmp/claude-1000/-home-dva-workspace-docklog/f9022b92-4536-49e5-8e38-c24f296b0368/scratchpad/vl/victoria-logs-prod bash -c 'timeout 3 scripts/run-dev.sh vl 2>&1 | head -5 || true'`
Expected: `bash -n` 無語法錯;啟動輸出含 `auth:  Basic Auth 已開(user=admin)`。

- [ ] **Step 5: Commit**

```bash
git add scripts/run-dev.sh README.md
git commit -m "docs: VL Basic Auth 執行示範與 env 說明(含 TLS caveat)

Claude-Session: https://claude.ai/code/session_018usf3iQoLwJuvsjzPQmXDN"
```

---

## Self-Review

**Spec coverage:**
- spec §1 VL ops/文件 → Task 3(run-dev.sh `-httpAuth.*` + README 示範)✓
- spec §2 寫入端 client → Task 1(移除 APIKey、加 Username/Password、SetBasicAuth)✓
- spec §3 讀取端 vl/main → Task 2(NewWithAuth、Ping/Query 帶 auth、main 讀 env)✓
- spec §4 測試 → Task 1(httptest 斷言 header)+ Task 2(真 VL 端到端 401/ok)✓
- spec §5 caveat → Task 3(README)✓
- **與 spec 的偏差(已於 handoff 標註)**:spec 寫「`New` 簽章增 user/pass」,plan 改為「新增 `NewWithAuth`、`New` 不動」。行為等價,但避免波及 13 個 `vl.New` 呼叫點(更手術)。

**Placeholder scan:** 無 TBD/TODO;所有 code step 含完整程式碼與確切指令。

**Type consistency:** `RemoteConfig.Username/Password`(Task 1 產出)與 Task 3 文件引用一致;`vl.NewWithAuth`、`vltest.StartVLWithAuth` 在 Task 2 Produces 與其測試/main 使用處簽章一致;`auth(req)` helper 內部一致。base64("u:p")="dTpw" 已驗算(u=0x75 :=0x3a p=0x70 → dTpw,無 padding)。
