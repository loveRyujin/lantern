// Command lantern-seed 生成星禾、云杉两个示例工作区。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/loveRyujin/lantern/internal/seed"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "lantern-seed: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("lantern-seed", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", "", "应用数据目录，默认是本机 Lantern 数据目录")
	seedN := fs.Uint64("seed", seed.DefaultSeed, "随机种子")
	force := fs.Bool("force", false, "星禾或云杉已存在时删除后重建")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "用法: lantern-seed [-root dir] [-seed n] [-force]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("不接受位置参数")
	}
	res, err := seed.Run(context.Background(), seed.Options{
		Root:  *root,
		Seed:  *seedN,
		Force: *force,
	})
	if err != nil {
		return err
	}
	for _, ws := range res.Workspaces {
		fmt.Printf("%s\n", ws.ID)
		fmt.Printf("  目录: %s\n", ws.Dir)
		fmt.Printf("  用户: %d\n", ws.Users)
		fmt.Printf("  商品: %d\n", ws.Products)
		fmt.Printf("  订单: %d\n", ws.Orders)
		fmt.Printf("  明细: %d\n", ws.OrderItems)
		fmt.Printf("  退款: %d\n", ws.Refunds)
		fmt.Printf("  哈希: %s\n", ws.Hash)
	}
	fmt.Printf("内容哈希: %s\n", res.Hash)
	fmt.Printf("数据目录: %s\n", res.Root)
	fmt.Printf("随机种子: %d\n", res.Seed)
	return nil
}
