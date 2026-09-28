// Command hashpassword 生成可写进 configs/config.yaml 的 bcrypt 密码哈希。
//
// 密码优先从标准输入读取（管道或交互），命令行参数只是兜底：
// 参数会留在 shell history 与 `ps` 输出里，而 server.auth.users 是长期存在的凭据。
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	cost := flag.Int("cost", bcrypt.DefaultCost, "bcrypt cost, 10-15 is the practical range")
	password := flag.String("password", "", "password to hash; prefer piping via stdin, this stays in shell history")
	flag.Parse()

	plain, err := readSecret(*password)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hashpassword:", err)
		os.Exit(1)
	}

	hash, err := generateHash(plain, *cost)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hashpassword:", err)
		os.Exit(1)
	}

	fmt.Println(string(hash))
}

// readSecret 优先用 stdin：有内容则取第一行（允许 `echo -n secret | hashpassword`），
// 交互式终端提示输入；-password 只在两者都拿不到时兜底。
func readSecret(fromFlag string) (string, error) {
	if stat, err := os.Stdin.Stat(); err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read stdin failed: %w", err)
		}
		if trimmed := strings.TrimRight(line, "\r\n"); trimmed != "" {
			return trimmed, nil
		}
	}

	if fromFlag != "" {
		return fromFlag, nil
	}

	fmt.Print("password: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read password failed: %w", err)
	}
	trimmed := strings.TrimRight(line, "\r\n")
	if trimmed == "" {
		return "", errors.New("password must not be empty")
	}
	return trimmed, nil
}

// generateHash 按指定 cost 生成哈希；cost 越界直接拒绝而不是悄悄改掉。
func generateHash(plain string, cost int) ([]byte, error) {
	if plain == "" {
		return nil, errors.New("password must not be empty")
	}
	// bcrypt 只用前 72 字节，超出的部分被忽略——写进配置却自己截断是很难查的坑
	if len(plain) > 72 {
		return nil, fmt.Errorf("password is %d bytes, bcrypt truncates at 72; use a shorter secret", len(plain))
	}
	if cost < bcrypt.MinCost || cost > 15 {
		return nil, fmt.Errorf("cost %d out of range, use %d-15", cost, bcrypt.MinCost)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(plain), cost)
	if err != nil {
		return nil, fmt.Errorf("hash generation failed: %w", err)
	}
	return hash, nil
}
