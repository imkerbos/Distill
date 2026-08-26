// Package manifests_test 校验仓库里的 Kubernetes 清单能被 k8s.io/api 的类型
// 严格解出来。
//
// 为什么要有它：清单是纯文本，写错一个字段名不会有任何症状 —— 直到某个人在
// 一次真实部署里 apply 它，而那时错的那一行往往是 securityContext 或
// readOnlyRootFilesystem 这类"少了也能起来"的收敛项。strict 模式把未知字段
// 变成编译期之外唯一能自动发现它的地方。
//
// 本测试不连集群、不需要 kubectl。
package manifests_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes/scheme"
)

// manifestGlobs 是所有会被交给操作者 apply 的清单。
//
// 用显式的 glob 而不是"扫描整个仓库的 yaml"：后者会把 CI 配置、前端配置和
// conformance 用的临时工作负载一并拖进来，而它们不是部署清单。
var manifestGlobs = []string{
	"../../deploy/kubernetes/*.yaml",
	"../../docs/deploy/*.yaml",
}

func TestManifestsDecodeStrictly(t *testing.T) {
	codecs := serializer.NewCodecFactory(scheme.Scheme, serializer.EnableStrict)
	decoder := codecs.UniversalDeserializer()

	var files []string
	for _, glob := range manifestGlobs {
		matched, err := filepath.Glob(glob)
		if err != nil {
			t.Fatalf("glob %s: %v", glob, err)
		}
		files = append(files, matched...)
	}
	if len(files) == 0 {
		t.Fatal("no manifests found; the globs are stale")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path) //nolint:gosec // G304: path comes from manifestGlobs, a constant in this file.
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var kinds int
			for _, doc := range strings.Split(string(raw), "\n---") {
				if strings.TrimSpace(stripComments(doc)) == "" {
					continue
				}
				if _, _, err := decoder.Decode([]byte(doc), nil, nil); err != nil {
					t.Errorf("decode: %v", err)
					continue
				}
				kinds++
			}
			if kinds == 0 {
				t.Error("file holds no Kubernetes object")
			}
		})
	}
}

// stripComments 去掉整行注释，用来判断一个分段是否只有注释。
//
// 只判断"空不空"，不参与解码 —— 解码拿到的仍是原文，注释由 YAML 解析器
// 自己处理。
func stripComments(doc string) string {
	var b strings.Builder
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
