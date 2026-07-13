package client

import (
	"encoding/json"
	"log/slog"
	"testing"
)

// pgxLikeConfig 模擬 pgconn.Config:帶一個函式欄位,json.Marshal 會失敗。
type pgxLikeConfig struct {
	Host    string
	DialFvn func() error // json 無法序列化 -> 觸發原 bug
}

// 非序列化的 attr 值(函式 / 帶函式欄位的結構)必須被強制成可 JSON 序列化的形式,
// 否則整批 log 會在 encodeNDJSON 失敗時被靜默丟棄(違反契約 + 掉 log)。
func TestFlattenAttrCoercesNonSerializable(t *testing.T) {
	cases := []struct {
		name string
		val  any
	}{
		{"func", func() {}},
		{"struct-with-func", pgxLikeConfig{Host: "db", DialFvn: func() error { return nil }}},
		{"chan", make(chan int)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dst := map[string]any{}
			flattenAttr(dst, "", slog.Any("k", c.val))
			if _, err := json.Marshal(dst); err != nil {
				t.Fatalf("flatten 後必須可 JSON 序列化,得: %v", err)
			}
			if _, ok := dst["k"].(string); !ok {
				t.Fatalf("非序列化值應轉成字串,得 %T", dst["k"])
			}
		})
	}
}

// 可序列化的值必須原樣保留(不因修正被無謂字串化)。
func TestFlattenAttrKeepsSerializable(t *testing.T) {
	dst := map[string]any{}
	flattenAttr(dst, "", slog.String("s", "x"))
	flattenAttr(dst, "", slog.Int("n", 5))
	flattenAttr(dst, "", slog.Any("m", map[string]int{"a": 1}))
	if dst["s"] != "x" {
		t.Errorf("string 值變了: %v", dst["s"])
	}
	if dst["n"] != int64(5) {
		t.Errorf("int 值變了: %v (%T)", dst["n"], dst["n"])
	}
	if _, err := json.Marshal(dst); err != nil {
		t.Fatalf("可序列化值仍須可序列化: %v", err)
	}
	if _, ok := dst["m"].(map[string]int); !ok {
		t.Errorf("可序列化的 map 應原樣保留,得 %T", dst["m"])
	}
}
