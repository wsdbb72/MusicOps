// Package artistnorm normalizes artist credits for grouping and matching.
// It only folds display variants that identify the same act; it does not
// translate arbitrary names or guess relationships between artists.
package artistnorm

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

type aliasGroup struct {
	canonical string
	aliases   []string
}

var aliasGroups = []aliasGroup{
	{canonical: "周杰伦", aliases: []string{"周杰伦", "周傑倫", "Jay Chou", "JayChou", "Zhou Jielun", "ZhouJielun"}},
	{canonical: "陈奕迅", aliases: []string{"陈奕迅", "陳奕迅", "Eason Chan", "EasonChan"}},
	{canonical: "林俊杰", aliases: []string{"林俊杰", "林俊傑", "JJ Lin", "JJLin"}},
	{canonical: "蔡依林", aliases: []string{"蔡依林", "Jolin Tsai", "JolinTsai"}},
	{canonical: "王力宏", aliases: []string{"王力宏", "Leehom Wang", "Wang Leehom", "LeehomWang", "WangLeehom"}},
	{canonical: "孙燕姿", aliases: []string{"孙燕姿", "孫燕姿", "Stefanie Sun", "StefanieSun"}},
	{canonical: "张学友", aliases: []string{"张学友", "張學友", "Jacky Cheung", "JackyCheung"}},
	{canonical: "刘德华", aliases: []string{"刘德华", "劉德華", "Andy Lau", "AndyLau"}},
	{canonical: "张惠妹", aliases: []string{"张惠妹", "張惠妹", "A-Mei", "AMei", "A Mei"}},
	{canonical: "邓紫棋", aliases: []string{"邓紫棋", "鄧紫棋", "G.E.M.", "G.E.M", "Gloria Tang", "GloriaTang"}},
	{canonical: "五月天", aliases: []string{"五月天", "Mayday"}},
	{canonical: "陶喆", aliases: []string{"陶喆", "陶吉吉", "David Tao", "DavidTao"}},
}

var canonicalByKey = func() map[string]string {
	out := make(map[string]string)
	for _, group := range aliasGroups {
		canonical := strings.TrimSpace(group.canonical)
		for _, alias := range append([]string{canonical}, group.aliases...) {
			if key := looseKey(alias); key != "" {
				out[key] = canonical
			}
		}
	}
	return out
}()

var hanVariants = map[rune]rune{
	'張': '张', '學': '学', '劉': '刘', '陳': '陈', '楊': '杨', '黃': '黄',
	'趙': '赵', '吳': '吴', '鄭': '郑', '謝': '谢', '鐘': '钟', '鍾': '钟',
	'鄧': '邓', '許': '许', '羅': '罗', '葉': '叶', '蘇': '苏', '賈': '贾',
	'盧': '卢', '馬': '马', '龍': '龙', '鳳': '凤', '傑': '杰', '倫': '伦',
	'華': '华', '語': '语', '樂': '乐', '愛': '爱', '願': '愿', '後': '后',
	'間': '间', '觸': '触', '電': '电', '搖': '摇', '時': '时', '說': '说',
	'會': '会', '來': '来', '為': '为', '無': '无', '與': '与', '這': '这',
	'過': '过', '還': '还', '將': '将', '對': '对', '萬': '万', '歲': '岁',
	'長': '长', '場': '场', '飛': '飞', '車': '车', '門': '门', '風': '风',
	'雲': '云', '見': '见', '實': '实', '現': '现', '開': '开', '關': '关',
	'發': '发', '頭': '头', '體': '体', '聽': '听', '讀': '读', '書': '书',
	'畫': '画', '數': '数', '當': '当', '樣': '样', '讓': '让', '從': '从',
}

// Canonical returns the reviewed display spelling for a known alias, or the
// original text when no identity evidence exists.
func Canonical(value string) string {
	value = strings.TrimSpace(norm.NFKC.String(value))
	if value == "" {
		return ""
	}
	if canonical, ok := canonicalByKey[looseKey(value)]; ok {
		return canonical
	}
	if hanNameOnly(value) {
		return simplifyHan(value)
	}
	return value
}

// Key is the stable grouping key. Callers keep the original credit as an
// alias/display value.
func Key(value string) string {
	return looseKey(Canonical(value))
}

// Same reports whether two single-artist credits are equivalent.
func Same(left, right string) bool {
	leftKey, rightKey := Key(left), Key(right)
	return leftKey != "" && leftKey == rightKey
}

func looseKey(value string) string {
	value = strings.ToLower(norm.NFKC.String(strings.TrimSpace(value)))
	var out strings.Builder
	for _, r := range value {
		if simplified, ok := hanVariants[r]; ok {
			r = simplified
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func hanNameOnly(value string) bool {
	seen := false
	for _, r := range value {
		switch {
		case unicode.Is(unicode.Han, r):
			seen = true
		case unicode.IsSpace(r):
		default:
			return false
		}
	}
	return seen
}

func simplifyHan(value string) string {
	return strings.Map(func(r rune) rune {
		if simplified, ok := hanVariants[r]; ok {
			return simplified
		}
		return r
	}, value)
}
