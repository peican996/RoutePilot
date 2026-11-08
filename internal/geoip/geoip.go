// Package geoip 把 IP 映射到国家/地区与大致经纬度。
//
// 数据源是我们随包分发的 geoip.metadb（MaxMind DB 格式），离线可用、
// 不依赖任何在线 GeoIP API——机场节点和目标地址的定位不能靠联网。
package geoip

import (
	"net"
	"strings"

	"github.com/oschwald/maxminddb-golang"
)

type DB struct {
	db *maxminddb.Reader
}

// Load 从内存加载 MMDB。data 一般是 embed 进二进制的 geoip.metadb。
func Load(data []byte) (*DB, error) {
	r, err := maxminddb.FromBytes(data)
	if err != nil {
		return nil, err
	}
	return &DB{db: r}, nil
}

// Country 返回 IP 所属的 ISO 3166-1 alpha-2 国家码（大写）。
// 查不到或不是合法 IP 时返回空串。
func (d *DB) Country(ipStr string) string {
	if d == nil {
		return ""
	}
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil {
		return ""
	}
	var rec map[string]any
	if err := d.db.Lookup(ip, &rec); err != nil {
		return ""
	}
	return extractCode(rec)
}

// Clash 的 geoip.metadb 记录形如 {"country": {"iso_code": "US"}}。
// 换个来源的库可能叫 code / iso，都兜一下，免得为一个字段名就崩。
func extractCode(rec map[string]any) string {
	if c, ok := rec["country"].(map[string]any); ok {
		for _, k := range []string{"iso_code", "code", "iso"} {
			if v, ok := c[k].(string); ok && v != "" {
				return strings.ToUpper(v)
			}
		}
	}
	for _, k := range []string{"code", "iso_code"} {
		if v, ok := rec[k].(string); ok && v != "" {
			return strings.ToUpper(v)
		}
	}
	return ""
}

// isoCoords 是国家/地区中心点的大致经纬度，用来在地图上落点。
// 用国家中心而不是首都，视觉上更自然（美国落在中部而不是华盛顿）。
var isoCoords = map[string][2]float64{
	"CN": {104, 35}, "HK": {114.2, 22.3}, "MO": {113.5, 22.2},
	"TW": {121, 23.7}, "JP": {138, 36}, "KR": {127.5, 36.5},
	"SG": {103.8, 1.3}, "MY": {102, 4}, "TH": {100.5, 15},
	"VN": {107, 14}, "PH": {122, 13}, "ID": {117, -2},
	"IN": {79, 22}, "PK": {69, 30}, "TR": {35, 39},
	"AE": {54, 24}, "IL": {35, 31.5}, "SA": {45, 24},
	"US": {-98, 39}, "CA": {-105, 55}, "MX": {-102, 23},
	"BR": {-50, -12}, "AR": {-64, -34}, "CL": {-71, -33},
	"CO": {-74, 4}, "GB": {-2, 54}, "IE": {-8, 53},
	"FR": {2, 46.8}, "DE": {10.5, 51}, "NL": {5.5, 52.2},
	"BE": {4.5, 50.8}, "LU": {6.1, 49.8}, "CH": {8.2, 46.8},
	"AT": {14.5, 47.7}, "IT": {12.5, 43}, "ES": {-3.7, 40.3},
	"PT": {-8, 39.6}, "PL": {19, 52}, "CZ": {15.5, 49.8},
	"SE": {16, 62}, "NO": {9, 62}, "FI": {26, 64},
	"DK": {9.5, 56}, "RU": {60, 58},
	"UA": {31, 49}, "RO": {25, 46}, "GR": {22, 39},
	"BG": {25, 42.8}, "HU": {19, 47}, "HR": {16.5, 45.5},
	"RS": {20, 44}, "AU": {134, -25}, "NZ": {173, -41},
	"ZA": {24, -29}, "EG": {30, 26}, "NG": {8, 10},
	"KE": {38, 0.5}, "MA": {-6, 32}, "DZ": {2, 28},
	"IS": {-18.5, 64.9}, "CY": {33, 35},
	"MT": {14.4, 35.9}, "EE": {25.5, 58.7}, "LV": {25, 56.9},
	"LT": {23.8, 55.2}, "SK": {19.5, 48.7}, "SI": {14.8, 46.1},
	"BY": {28, 53.7}, "KZ": {67, 48}, "UZ": {64, 41.3},
	"GE": {43.5, 42}, "AM": {45, 40.3}, "AZ": {47.5, 40.2},
	"MN": {104, 47}, "LA": {102, 19.5}, "KH": {104.8, 12.6},
	"MM": {96, 21.5}, "BD": {90, 24}, "LK": {80.7, 7.6},
	"NP": {84, 28.2}, "AF": {66, 34}, "IR": {53, 32.5},
	"IQ": {43, 33}, "SY": {38.5, 35}, "JO": {36, 31.2},
	"KW": {47.8, 29.3}, "QA": {51.2, 25.3}, "OM": {56, 21},
	"YE": {47.5, 15.5},
}

// Location 给出国家码对应的经纬度。查不到返回 ok=false。
func Location(iso string) (lon, lat float64, ok bool) {
	if iso == "" {
		return 0, 0, false
	}
	c, found := isoCoords[strings.ToUpper(iso)]
	if !found {
		return 0, 0, false
	}
	return c[0], c[1], true
}
