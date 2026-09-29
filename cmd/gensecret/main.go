// Command gensecret 生成 Web 控制台的 HS256 签名密钥。
//
// 密钥只写进环境变量 GODELAYQ_SERVER_AUTH_JWT_SECRET，不进配置文件：
// config.yaml 会提交，密钥一旦落盘就等于长期凭据泄漏。
package main

import (
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
)

const envKey = "GODELAYQ_SERVER_AUTH_JWT_SECRET"

func main() {
	n := flag.Int("bytes", 32, "随机字节数，base64url 编码后的字符数会更长")
	flag.Parse()

	secret, err := generate(*n)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gensecret:", err)
		os.Exit(1)
	}

	// 密钥走 stdout、提示走 stderr，`export X=$(go run ./cmd/gensecret)` 才拿得到纯密钥
	fmt.Println(secret)
	fmt.Fprintf(os.Stderr, `
# bash / Git Bash
export %[1]s=%[2]s

# PowerShell
$env:%[1]s = '%[2]s'

注入后重启进程生效；轮换密钥会让全部已签发令牌立即失效（所有人需重新登录）。
`, envKey, secret)
}

// generate 返回 base64url（无填充）编码的随机密钥。
func generate(n int) (string, error) {
	// 下限对齐 core.minJWTSecretLen：短密钥可被离线爆破，签出来的令牌等于没签
	if n < 32 || n > 128 {
		return "", fmt.Errorf("bytes must be within 32-128, got %d", n)
	}

	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("random generation failed: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
