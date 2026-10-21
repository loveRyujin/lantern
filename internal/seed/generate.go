package seed

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

const (
	statusPaid      = "已支付"
	statusCancelled = "已取消"
	statusPending   = "待支付"

	// 订单 created_at 落在 [orderWindowStart, orderWindowEnd)。整 12 个月，UTC。
	// 退款可以比订单晚最多 20 天，因此可能落到窗口之后。
)

var (
	orderWindowStart = time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC)
	orderWindowEnd   = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
)

// contact 是要写入 sensitive.db 的一行。分析库不保存这两列。
type contact struct {
	UserID string
	Phone  string
	Email  string
}

type userRow struct {
	ID    string
	Name  string
	City  string
	Level string
	Phone string
	Email string
}

type productRow struct {
	ID         string
	Name       string
	Category   string
	PriceCents int64
}

type orderRow struct {
	ID          string
	UserID      string
	CreatedAt   string
	Status      string
	AmountCents int64
}

type itemRow struct {
	ID             string
	OrderID        string
	ProductID      string
	Qty            int64
	UnitPriceCents int64
}

type refundRow struct {
	ID          string
	OrderID     string
	CreatedAt   string
	AmountCents int64
	Reason      string
}

type dataset struct {
	users    []userRow
	products []productRow
	orders   []orderRow
	items    []itemRow
	refunds  []refundRow
}

func (d dataset) contacts() []contact {
	out := make([]contact, len(d.users))
	for i, u := range d.users {
		out[i] = contact{UserID: u.ID, Phone: u.Phone, Email: u.Email}
	}
	return out
}

type category struct {
	Name     string
	Nouns    []string
	MinPrice int64
	MaxPrice int64
}

type profile struct {
	ID             string
	Prefix         string
	EmailDomain    string
	Users          int
	Products       int
	Orders         int
	PaidPermille   int
	CancelPermille int
	RefundPermille int
	Categories     []category
	Cities         []string
	Levels         []string
	Reasons        []string
}

func (p profile) validate() error {
	if p.ID == "" || p.Prefix == "" || p.EmailDomain == "" {
		return errors.New("seed: 工作区配置不完整")
	}
	if p.Users <= 0 || p.Products <= 0 || p.Orders <= 0 {
		return errors.New("seed: 规模必须为正")
	}
	if p.PaidPermille < 0 || p.CancelPermille < 0 || p.RefundPermille < 0 ||
		p.PaidPermille+p.CancelPermille > 1000 || p.RefundPermille > 1000 {
		return errors.New("seed: 比例超出范围")
	}
	if len(p.Categories) == 0 || len(p.Cities) == 0 || len(p.Levels) == 0 || len(p.Reasons) == 0 {
		return errors.New("seed: 目录为空")
	}
	for _, c := range p.Categories {
		if c.Name == "" || len(c.Nouns) == 0 || c.MinPrice <= 0 || c.MaxPrice < c.MinPrice {
			return fmt.Errorf("seed: 品类 %s 配置无效", c.Name)
		}
	}
	return nil
}

func generate(p profile, seedN uint64) (dataset, error) {
	if err := p.validate(); err != nil {
		return dataset{}, err
	}
	rng := newRNG(seedN, p.ID)
	users, err := genUsers(rng, p)
	if err != nil {
		return dataset{}, err
	}
	products := genProducts(rng, p)
	orders, items, refunds, err := genOrders(rng, p, users, products)
	if err != nil {
		return dataset{}, err
	}
	return dataset{
		users:    users,
		products: products,
		orders:   orders,
		items:    items,
		refunds:  refunds,
	}, nil
}

func newRNG(seedN uint64, workspaceID string) *rand.Rand {
	sum := sha256.Sum256([]byte("lantern-seed:" + workspaceID))
	stream := binary.BigEndian.Uint64(sum[:8])
	return rand.New(rand.NewPCG(seedN, stream))
}

func genUsers(rng *rand.Rand, p profile) ([]userRow, error) {
	used := make(map[string]struct{}, p.Users)
	users := make([]userRow, 0, p.Users)
	for i := 1; i <= p.Users; i++ {
		id := formatID(p.Prefix, "u", i, 4)
		phone, err := uniquePhone(rng, used)
		if err != nil {
			return nil, err
		}
		users = append(users, userRow{
			ID:    id,
			Name:  surnames[rng.IntN(len(surnames))] + givenNames[rng.IntN(len(givenNames))],
			City:  p.Cities[rng.IntN(len(p.Cities))],
			Level: p.Levels[rng.IntN(len(p.Levels))],
			Phone: phone,
			Email: id + "@" + p.EmailDomain,
		})
	}
	return users, nil
}

func genProducts(rng *rand.Rand, p profile) []productRow {
	products := make([]productRow, 0, p.Products)
	for i := 1; i <= p.Products; i++ {
		cat := p.Categories[rng.IntN(len(p.Categories))]
		noun := cat.Nouns[rng.IntN(len(cat.Nouns))]
		adj := adjectives[rng.IntN(len(adjectives))]
		price := cat.MinPrice + rng.Int64N(cat.MaxPrice-cat.MinPrice+1)
		products = append(products, productRow{
			ID:         formatID(p.Prefix, "p", i, 4),
			Name:       adj + noun,
			Category:   cat.Name,
			PriceCents: price,
		})
	}
	return products
}

func genOrders(rng *rand.Rand, p profile, users []userRow, products []productRow) ([]orderRow, []itemRow, []refundRow, error) {
	orders := make([]orderRow, 0, p.Orders)
	items := make([]itemRow, 0, p.Orders*2)
	refunds := make([]refundRow, 0, p.Orders/10)
	itemN := 0
	refundN := 0
	for i := 1; i <= p.Orders; i++ {
		user := users[rng.IntN(len(users))]
		created := randomTime(rng)
		status := pickStatus(rng, p)
		nItems := 1 + rng.IntN(3)
		orderID := formatID(p.Prefix, "o", i, 6)
		var amount int64
		for range nItems {
			itemN++
			prod := products[rng.IntN(len(products))]
			qty := int64(1 + rng.IntN(3))
			factor := int64(90 + rng.IntN(11))
			unit := prod.PriceCents * factor / 100
			if unit < 1 {
				unit = 1
			}
			items = append(items, itemRow{
				ID:             formatID(p.Prefix, "i", itemN, 6),
				OrderID:        orderID,
				ProductID:      prod.ID,
				Qty:            qty,
				UnitPriceCents: unit,
			})
			amount += qty * unit
		}
		orders = append(orders, orderRow{
			ID:          orderID,
			UserID:      user.ID,
			CreatedAt:   created,
			Status:      status,
			AmountCents: amount,
		})
		if status != statusPaid || rng.IntN(1000) >= p.RefundPermille {
			continue
		}
		refundN++
		row, err := genRefund(rng, p, refundN, orders[len(orders)-1])
		if err != nil {
			return nil, nil, nil, err
		}
		refunds = append(refunds, row)
	}
	return orders, items, refunds, nil
}

func genRefund(rng *rand.Rand, p profile, n int, order orderRow) (refundRow, error) {
	ordered, err := time.Parse(time.RFC3339, order.CreatedAt)
	if err != nil {
		return refundRow{}, fmt.Errorf("seed: 订单时间: %w", err)
	}
	days := 1 + rng.IntN(20)
	extra := rng.IntN(24 * 60 * 60)
	at := ordered.Add(time.Duration(days)*24*time.Hour + time.Duration(extra)*time.Second).UTC()
	pct := int64(30 + rng.IntN(71))
	amount := order.AmountCents * pct / 100
	if amount < 1 {
		amount = 1
	}
	if amount > order.AmountCents {
		amount = order.AmountCents
	}
	return refundRow{
		ID:          formatID(p.Prefix, "r", n, 6),
		OrderID:     order.ID,
		CreatedAt:   at.Format(time.RFC3339),
		AmountCents: amount,
		Reason:      p.Reasons[rng.IntN(len(p.Reasons))],
	}, nil
}

func pickStatus(rng *rand.Rand, p profile) string {
	roll := rng.IntN(1000)
	switch {
	case roll < p.PaidPermille:
		return statusPaid
	case roll < p.PaidPermille+p.CancelPermille:
		return statusCancelled
	default:
		return statusPending
	}
}

func randomTime(rng *rand.Rand) string {
	span := orderWindowEnd.Unix() - orderWindowStart.Unix()
	sec := orderWindowStart.Unix() + rng.Int64N(span)
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

func uniquePhone(rng *rand.Rand, used map[string]struct{}) (string, error) {
	for range 64 {
		prefix := phonePrefixes[rng.IntN(len(phonePrefixes))]
		phone := prefix + fmt.Sprintf("%08d", rng.IntN(100_000_000))
		if _, ok := used[phone]; ok {
			continue
		}
		used[phone] = struct{}{}
		return phone, nil
	}
	return "", errors.New("seed: 手机号重复过多")
}

func formatID(prefix, kind string, n, width int) string {
	return fmt.Sprintf("%s-%s-%0*d", prefix, kind, width, n)
}

var phonePrefixes = []string{
	"130", "131", "132", "133", "135", "136", "137", "138", "139",
	"150", "151", "152", "157", "158", "159",
	"180", "186", "187", "188", "189",
}

var surnames = []string{
	"陈", "林", "周", "吴", "郑", "王", "李", "张", "刘", "黄",
	"赵", "孙", "马", "朱", "胡", "高", "何", "郭",
}

var givenNames = []string{
	"一诺", "子轩", "雨桐", "浩然", "欣怡", "俊杰", "诗涵", "博文",
	"思远", "嘉琪", "明哲", "婉清", "天宇", "若溪", "景行", "安然",
	"清和", "知夏", "予安", "书衡", "念真", "望舒", "承宇", "疏影",
}

var adjectives = []string{"简约", "轻奢", "经典", "家用", "升级", "基础", "年度", "手作"}

// 区域不单列。口径手册按城市归到华东、华南、华北、西南、华中。
var cities = []string{
	"上海", "杭州", "南京", "宁波", "苏州",
	"广州", "深圳", "厦门", "佛山",
	"北京", "天津", "青岛",
	"成都", "重庆", "昆明",
	"武汉", "长沙", "郑州",
}

var profiles = []profile{
	{
		ID:             "星禾",
		Prefix:         "xh",
		EmailDomain:    "xinghe.example",
		Users:          240,
		Products:       48,
		Orders:         3600,
		PaidPermille:   820,
		CancelPermille: 110,
		RefundPermille: 150,
		Cities:         cities,
		Levels:         []string{"普通", "银卡", "金卡", "钻石"},
		Reasons:        []string{"七天无理由", "质量问题", "尺码不合适", "发错货", "不想要了"},
		Categories: []category{
			{Name: "服饰", Nouns: []string{"针织衫", "衬衫", "连衣裙", "夹克", "运动鞋"}, MinPrice: 7900, MaxPrice: 69900},
			{Name: "数码", Nouns: []string{"耳机", "键盘", "充电宝", "鼠标"}, MinPrice: 9900, MaxPrice: 399900},
			{Name: "美妆", Nouns: []string{"面霜", "口红", "防晒", "精华"}, MinPrice: 4900, MaxPrice: 45900},
			{Name: "食品", Nouns: []string{"咖啡豆", "坚果", "茶叶", "巧克力"}, MinPrice: 1990, MaxPrice: 15900},
			{Name: "家居", Nouns: []string{"收纳盒", "床单", "香薰", "抱枕"}, MinPrice: 2990, MaxPrice: 29900},
		},
	},
	{
		ID:             "云杉",
		Prefix:         "ys",
		EmailDomain:    "yunshan.example",
		Users:          96,
		Products:       30,
		Orders:         1200,
		PaidPermille:   880,
		CancelPermille: 70,
		RefundPermille: 80,
		Cities:         cities,
		Levels:         []string{"普通", "会员", "设计师"},
		Reasons:        []string{"尺寸不合适", "有色差", "物流破损", "七天无理由", "不想要了"},
		Categories: []category{
			{Name: "沙发", Nouns: []string{"布艺沙发", "皮沙发", "单人沙发", "沙发床"}, MinPrice: 199900, MaxPrice: 899900},
			{Name: "床具", Nouns: []string{"床架", "床垫", "床头柜", "四件套"}, MinPrice: 49900, MaxPrice: 599900},
			{Name: "桌椅", Nouns: []string{"餐桌", "书桌", "餐椅", "书柜"}, MinPrice: 29900, MaxPrice: 399900},
			{Name: "灯具", Nouns: []string{"吊灯", "落地灯", "台灯", "壁灯"}, MinPrice: 9900, MaxPrice: 159900},
			{Name: "收纳", Nouns: []string{"衣柜", "斗柜", "置物架", "鞋架"}, MinPrice: 19900, MaxPrice: 259900},
		},
	},
}
