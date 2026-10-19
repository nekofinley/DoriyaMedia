package metadata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"doriyamedia/internal/config"
)

// The matcher must accept real titles and reject "close enough" nonsense —
// a wrong rename is worse than no metadata at all.
func TestPickBGM(t *testing.T) {
	subj := func(cn, orig string, year int) BgmSubject {
		return BgmSubject{NameCN: cn, Name: orig, Year: year}
	}
	cases := []struct {
		name    string
		query   string
		year    int
		results []BgmSubject
		want    int
	}{
		{
			name:    "中文名精确匹配",
			query:   "孤独摇滚",
			results: []BgmSubject{subj("孤独摇滚！", "ぼっち・ざ・ろっく！", 2022), subj("孤独", "", 1959)},
			want:    0,
		},
		{
			name:    "日文原名匹配",
			query:   "ぼっち・ざ・ろっく",
			results: []BgmSubject{subj("孤独摇滚！", "ぼっち・ざ・ろっく！", 2022)},
			want:    0,
		},
		{
			name:    "去掉标点后精确",
			query:   "86 不存在的战区",
			results: []BgmSubject{subj("86-不存在的战区-", "86-エイティシックス-", 2021)},
			want:    0,
		},
		{
			name:    "续作不会被主条目抢走(精确优先)",
			query:   "咒术回战",
			results: []BgmSubject{subj("咒术回战", "呪術廻戦", 2020), subj("咒术回战 怀玉･玉折 / 涩谷事变", "", 2023)},
			want:    0,
		},
		{
			name:    "虚构电影名不能变成 Another",
			query:   "Mankai Movie A3!: Another Stories",
			year:    2021,
			results: []BgmSubject{subj("Another", "アナザー", 2012)},
			want:    -1,
		},
		{
			name:    "非番剧名字不能乱配",
			query:   "Dette er en test show",
			results: []BgmSubject{subj("Computer Test", "", 2020)},
			want:    -1,
		},
		{
			name:    "真人电影名不该匹配到无关番剧",
			query:   "流浪地球",
			results: []BgmSubject{subj("来自深渊 漂泊之黄昏", "メイドインアビス", 2019)},
			want:    -1,
		},
		{
			name:    "没有结果",
			query:   "某个不存在的名字",
			results: nil,
			want:    -1,
		},
		{
			name:    "罗马音目录名暂时匹配不到(已知取舍,交给 TMDB)",
			query:   "Kekkai Sensen",
			results: []BgmSubject{subj("血界戦線", "血界戦線", 2015)},
			want:    -1,
		},
		{
			name:    "文件夹多打一个字也能认出来",
			query:   "别再当欧尼酱了",
			results: []BgmSubject{subj("别当欧尼酱了！", "お兄ちゃんはおしまい！", 2023)},
			want:    0,
		},
		{
			name:    "常见错字(学院/学园)也能认出来",
			query:   "监狱学院",
			results: []BgmSubject{subj("监狱学园", "監獄学園", 2015), subj("监狱兔 第一季", "Usavich", 2006)},
			want:    0,
		},
		{
			name:    "少一个字的长标题也能认出来",
			query:   "转生成蜘蛛又怎么样",
			results: []BgmSubject{subj("转生成蜘蛛又怎样！", "蜘蛛ですが、なにか?", 2021)},
			want:    0,
		},
		{
			name:    "近似匹配失效时,第一条搜索结果也不能硬套",
			query:   "别再当欧尼酱了",
			results: []BgmSubject{subj("", "凡人修仙传 年番", 2021), subj("别惹流氓兔马修", "", 2018)},
			want:    -1,
		},
		{
			name:    "同年前后季不因年份而误配",
			query:   "进击的巨人",
			year:    2019,
			results: []BgmSubject{subj("进击的巨人 第三季 Part.2", "進撃の巨人 Season 3 Part.2", 2019)},
			want:    -1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pickBGM(c.query, c.year, c.results); got != c.want {
				t.Fatalf("pickBGM(%q) = %d, want %d", c.query, got, c.want)
			}
		})
	}
}

func TestBGMQueryVariants(t *testing.T) {
	got := bgmQueryVariants("别再当欧尼酱了！")
	if len(got) == 0 || got[0] != "别当欧尼酱了" {
		t.Fatalf("bgmQueryVariants = %v, want first variant 别当欧尼酱了", got)
	}
	for _, v := range got {
		if len([]rune(v)) != len([]rune("别再当欧尼酱了"))-1 {
			t.Fatalf("variant %q has wrong length", v)
		}
	}
	// Latin titles are TMDB's job; deleting characters there only burns requests.
	if got := bgmQueryVariants("Kekkai Sensen"); got != nil {
		t.Fatalf("bgmQueryVariants(Latin) = %v, want nil", got)
	}
	if got := bgmQueryVariants("86"); got != nil {
		t.Fatalf("bgmQueryVariants(short) = %v, want nil", got)
	}
}

func TestSearchBGMFallsBackToShortenedQuery(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/search/subjects":
			var body struct {
				Keyword string `json:"keyword"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			seen = append(seen, body.Keyword)
			mu.Unlock()
			hits := []map[string]any{}
			switch body.Keyword {
			case "别再当欧尼酱了！":
				// The real engine returns unrelated anime for the longer title.
				hits = append(hits, map[string]any{"id": 348240, "name": "凡人修仙传 年番", "type": 2})
			case "别当欧尼酱了":
				hits = append(hits, map[string]any{
					"id": 378862, "name": "お兄ちゃんはおしまい！", "name_cn": "别当欧尼酱了！",
					"type": 2, "date": "2023-01-05", "eps": 12,
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": hits, "total": len(hits)})
		case "/v0/subjects/378862":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 378862, "name": "お兄ちゃんはおしまい！", "name_cn": "别当欧尼酱了！",
				"type": 2, "date": "2023-01-05", "eps": 12,
				"images": map[string]any{"large": "https://lain.bgm.tv/pic/cover/l/x.jpg"},
				"rating": map[string]any{"score": 7.6},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	m := NewManager(nil, &config.Config{BGM: config.BGMTV{APIBase: srv.URL}})
	sub, err := m.searchBGM(context.Background(), "别再当欧尼酱了！", 0)
	if err != nil {
		t.Fatalf("searchBGM: %v", err)
	}
	if sub == nil || sub.ID != 378862 {
		t.Fatalf("searchBGM = %+v, want subject 378862", sub)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || seen[0] != "别再当欧尼酱了！" || seen[1] != "别当欧尼酱了" {
		t.Fatalf("searches = %v, want the original then the shortened spelling", seen)
	}
}

func TestTitleClose(t *testing.T) {
	yes := [][2]string{
		{"别当欧尼酱了", "别再当欧尼酱了"},
		{"监狱学园", "监狱学院"},
		{"转生成蜘蛛又怎样", "转生成蜘蛛又怎么样"},
	}
	for _, c := range yes {
		if !titleClose(c[0], c[1]) {
			t.Fatalf("titleClose(%q, %q) = false, want true", c[0], c[1])
		}
	}
	no := [][2]string{
		{"another", "mankaimoviea3anotherstories"},
		{"computertest", "detteerentestshow"},
		{"来自深渊漂泊之黄昏", "流浪地球"},
		{"进击的巨人", "进击的巨人第三季part2"},
	}
	for _, c := range no {
		if titleClose(c[0], c[1]) {
			t.Fatalf("titleClose(%q, %q) = true, want false", c[0], c[1])
		}
	}
}

func TestPreferCN(t *testing.T) {
	if got := preferCN("孤独摇滚！", "ぼっち・ざ・ろっく！"); got != "孤独摇滚！" {
		t.Fatalf("preferCN = %q", got)
	}
	if got := preferCN("", "Kekkai Sensen"); got != "Kekkai Sensen" {
		t.Fatalf("preferCN fallback = %q", got)
	}
}
