// 一次性核查程序：拿 UAT 库直接跑平台自己的 PolicyPreview，落成 JSON。
// 只读，不写库，不经过 HTTP，因此不需要登录态。用完即删。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/mysqlregistry"
	"github.com/imkerbos/Distill/internal/store"
)

func main() {
	db, err := mysqlregistry.Open(config.DatabaseConfig{
		DSN: os.Getenv("DSN"), MaxOpenConns: 4, MaxIdleConns: 2,
		ConnMaxLifetime: 5 * time.Minute,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer db.Close()

	reg := mysqlregistry.New(db)
	r := collectstore.New(db, reg)

	from, _ := time.Parse(time.RFC3339, os.Getenv("FROM"))
	to, _ := time.Parse(time.RFC3339, os.Getenv("TO"))
	pv, err := r.PolicyPreview(context.Background(), os.Getenv("CLUSTER"), "",
		store.TimeWindow{From: from, To: to})
	if err != nil {
		fmt.Fprintln(os.Stderr, "preview:", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	if err := enc.Encode(pv); err != nil {
		fmt.Fprintln(os.Stderr, "encode:", err)
		os.Exit(1)
	}
}
