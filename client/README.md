# ducklog/client

純 stdlib 的 Go 傳輸層:一個 `slog.Handler`,把 log record 以 NDJSON
(每行一個 JSON 物件)POST 到後端,並支援 trace ID 傳遞。

Wire 格式(每行):

```json
{"ts":"<RFC3339Nano>","service":"...","level":"...","trace_id":"...","message":"...","attrs":{...}}
```

## 指向 VictoriaLogs(VL)

後端為 VictoriaLogs 時,直接打 VL 原生的 jsonline ingest endpoint —— 我們送的
NDJSON 與它原生相容,transport 程式碼不需改,只需設定 `Endpoint`:

```go
h := client.NewRemoteHandler(client.RemoteConfig{
    Endpoint: "http://<vl-host>:9428/insert/jsonline?_time_field=ts&_msg_field=message&_stream_fields=service",
    APIKey:   "", // VL OSS 無 auth
    Service:  "my-service",
})
log := slog.New(h)
defer h.Close()
```

Endpoint 的 query 參數把我們的 wire 欄位映射到 VL 的保留欄位:

| 參數 | 值 | 作用 |
| --- | --- | --- |
| `_time_field` | `ts` | 用我們的 `ts`(RFC3339Nano)當 VL 的 `_time` |
| `_msg_field` | `message` | 用我們的 `message` 當 VL 的 `_msg` |
| `_stream_fields` | `service` | 用 `service` 當 log stream 欄位 |

其餘欄位(`level`、`trace_id`)以同名欄位存入,可直接用 LogsQL 過濾,例如
`trace_id:="<uuid>"`。

### 巢狀 attrs 的處理

`attrs` 是巢狀 JSON 物件。VL ingest 時會把它**攤平成 dot-notation 欄位**:
`attrs={"host":"10.0.1.5"}` 存成欄位 `attrs.host="10.0.1.5"`,可用
`attrs.host:="10.0.1.5"` 查回。transport 端不需自行攤平。

## 查詢

VL 的查詢:`GET /select/logsql/query?query=<LogsQL>`,回傳 NDJSON(每列一個
JSON 物件)。例如查全部:`query=*`。

## 端到端測試

`e2e_test.go` 的 `TestEndToEndWireContract` 會啟一個真正的 VL 進程,實際 ingest
再查回驗證上述契約。需要 VL binary:

```bash
cd client
VL_BINARY=/path/to/victoria-logs-prod go test -run EndToEnd ./... -v
```

未設 `VL_BINARY`(或 binary 不存在)時測試會 skip;`go test -short ./...` 也會 skip。
