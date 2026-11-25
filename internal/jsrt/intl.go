package jsrt

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/dop251/goja"
)

type intlDateTimeConfig struct {
	Locale                 string
	LocaleFamily           string // "zh", "sv", "ja", "ko", "dmy", "en-us"
	TimeZone               string
	Loc                    *time.Location
	DateStyle              string
	TimeStyle              string
	Weekday                string
	Year                   string
	Month                  string
	Day                    string
	Hour                   string
	Minute                 string
	Second                 string
	FractionalSecondDigits int
	TimeZoneName           string
	Hour12                 bool
	HourCycle              string // "h11", "h12", "h23", "h24"
}

type intlPart struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

var (
	enWeekdaysLong   = [7]string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	enWeekdaysShort  = [7]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	enWeekdaysNarrow = [7]string{"S", "M", "T", "W", "T", "F", "S"}
	zhWeekdaysLong   = [7]string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}
	zhWeekdaysShort  = [7]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}
	zhWeekdaysNarrow = [7]string{"日", "一", "二", "三", "四", "五", "六"}

	enMonthsLong   = [12]string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
	enMonthsShort  = [12]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	enMonthsNarrow = [12]string{"J", "F", "M", "A", "M", "J", "J", "A", "S", "O", "N", "D"}
)

// installIntlGlobals installs a Go time/tzdata-backed Intl.DateTimeFormat and patches
// Date.prototype.toLocale{,Date,Time}String so explicit IANA timeZone options are always
// honored accurately (including DST) and invalid time zones fail fast with RangeError.
func (v *sessionVM) installIntlGlobals() {
	_ = v.vm.Set("__intlBuildDTF", func(call goja.FunctionCall) goja.Value {
		var localesVal, optsVal goja.Value
		mode := "date"
		if len(call.Arguments) > 0 {
			localesVal = call.Arguments[0]
		}
		if len(call.Arguments) > 1 {
			optsVal = call.Arguments[1]
		}
		if len(call.Arguments) > 2 && !goja.IsUndefined(call.Arguments[2]) && !goja.IsNull(call.Arguments[2]) {
			mode = call.Arguments[2].String()
		}
		cfg, errType, err := parseIntlDateTimeConfig(localesVal, optsVal, mode)
		if err != nil {
			if errType == "TypeError" {
				panic(v.vm.NewTypeError(err.Error()))
			}
			panic(v.newRangeError(err.Error()))
		}

		obj := v.vm.NewObject()
		_ = obj.Set("formatToParts", func(c goja.FunctionCall) goja.Value {
			ms, ok := extractEpochMillis(c)
			if !ok {
				panic(v.newRangeError("Invalid time value"))
			}
			t := time.UnixMilli(ms).In(cfg.Loc)
			parts := formatIntlDateTimeParts(t, cfg)
			out := make([]map[string]any, len(parts))
			for i, p := range parts {
				out[i] = map[string]any{"type": p.Type, "value": p.Value}
			}
			return v.vm.ToValue(out)
		})
		_ = obj.Set("format", func(c goja.FunctionCall) goja.Value {
			ms, ok := extractEpochMillis(c)
			if !ok {
				panic(v.newRangeError("Invalid time value"))
			}
			t := time.UnixMilli(ms).In(cfg.Loc)
			parts := formatIntlDateTimeParts(t, cfg)
			var sb strings.Builder
			for _, p := range parts {
				sb.WriteString(p.Value)
			}
			return v.vm.ToValue(sb.String())
		})
		_ = obj.Set("resolvedOptions", func(goja.FunctionCall) goja.Value {
			m := map[string]any{
				"locale":          cfg.Locale,
				"calendar":        "gregory",
				"numberingSystem": "latn",
				"timeZone":        cfg.TimeZone,
			}
			if cfg.DateStyle != "" {
				m["dateStyle"] = cfg.DateStyle
			}
			if cfg.TimeStyle != "" {
				m["timeStyle"] = cfg.TimeStyle
			}
			if cfg.Weekday != "" {
				m["weekday"] = cfg.Weekday
			}
			if cfg.Year != "" {
				m["year"] = cfg.Year
			}
			if cfg.Month != "" {
				m["month"] = cfg.Month
			}
			if cfg.Day != "" {
				m["day"] = cfg.Day
			}
			if cfg.Hour != "" {
				m["hour"] = cfg.Hour
				m["hour12"] = cfg.Hour12
				m["hourCycle"] = cfg.HourCycle
			}
			if cfg.Minute != "" {
				m["minute"] = cfg.Minute
			}
			if cfg.Second != "" {
				m["second"] = cfg.Second
			}
			if cfg.FractionalSecondDigits > 0 {
				m["fractionalSecondDigits"] = cfg.FractionalSecondDigits
			}
			if cfg.TimeZoneName != "" {
				m["timeZoneName"] = cfg.TimeZoneName
			}
			return v.vm.ToValue(m)
		})
		return obj
	})

	_, _ = v.vm.RunString(`
(() => {
const _buildDTF = globalThis.__intlBuildDTF;
delete globalThis.__intlBuildDTF;

function DateTimeFormat(locales, options) {
  if (!(this instanceof DateTimeFormat)) {
    return new DateTimeFormat(locales, options);
  }
  const impl = _buildDTF(locales, options, "date");
  this._impl = impl;
  this.format = (date) => impl.format(date === undefined ? Date.now() : Number(date));
  this.formatToParts = (date) => impl.formatToParts(date === undefined ? Date.now() : Number(date));
  this.resolvedOptions = () => impl.resolvedOptions();
}
DateTimeFormat.supportedLocalesOf = (locales) => {
  if (locales === undefined) return [];
  return Array.isArray(locales) ? locales.map(String) : [String(locales)];
};

globalThis.Intl = globalThis.Intl || {};
globalThis.Intl.DateTimeFormat = DateTimeFormat;

function makeDateLocaleMethod(mode) {
  return function(locales, options) {
    if (!(this instanceof Date)) {
      throw new TypeError("this is not a Date object.");
    }
    const ms = this.getTime();
    const impl = _buildDTF(locales, options, mode);
    if (!Number.isFinite(ms)) return "Invalid Date";
    return impl.format(ms);
  };
}
Date.prototype.toLocaleString = makeDateLocaleMethod("datetime");
Date.prototype.toLocaleDateString = makeDateLocaleMethod("date");
Date.prototype.toLocaleTimeString = makeDateLocaleMethod("time");
})();
`)
}

func (v *sessionVM) newRangeError(msg string) *goja.Object {
	if ctorVal := v.vm.Get("RangeError"); ctorVal != nil {
		if ctor, ok := goja.AssertConstructor(ctorVal); ok {
			if obj, err := ctor(nil, v.vm.ToValue(msg)); err == nil && obj != nil {
				return obj
			}
		}
	}
	return v.vm.NewTypeError(msg)
}

func extractEpochMillis(call goja.FunctionCall) (int64, bool) {
	if len(call.Arguments) == 0 || goja.IsUndefined(call.Arguments[0]) {
		return time.Now().UnixMilli(), true
	}
	f := call.Arguments[0].ToFloat()
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return int64(math.Trunc(f)), true
}

func parseIntlDateTimeConfig(localesVal, optsVal goja.Value, mode string) (intlDateTimeConfig, string, error) {
	locale := normalizeLocaleTag(localesVal)
	family := classifyLocaleFamily(locale)
	cfg := intlDateTimeConfig{
		Locale:       locale,
		LocaleFamily: family,
	}

	var rawOpts map[string]any
	if optsVal != nil && !goja.IsUndefined(optsVal) {
		if goja.IsNull(optsVal) {
			return cfg, "TypeError", fmt.Errorf("cannot convert undefined or null to object")
		}
		if m, ok := optsVal.Export().(map[string]any); ok {
			rawOpts = m
		}
	}

	getStr := func(key string) (string, bool) {
		if rawOpts == nil {
			return "", false
		}
		v, ok := rawOpts[key]
		if !ok || v == nil {
			return "", false
		}
		return strings.TrimSpace(fmt.Sprint(v)), true
	}

	if tzInput, hasTZ := getStr("timeZone"); hasTZ {
		if tzInput == "" {
			return cfg, "RangeError", fmt.Errorf("Invalid time zone specified: %q", tzInput)
		}
		loc, tzCanon, err := resolveTimeZone(tzInput)
		if err != nil {
			return cfg, "RangeError", err
		}
		cfg.Loc = loc
		cfg.TimeZone = tzCanon
	} else {
		cfg.Loc = time.Local
		cfg.TimeZone = time.Local.String()
	}

	checkEnum := func(key, val string, allowed ...string) error {
		for _, a := range allowed {
			if val == a {
				return nil
			}
		}
		return fmt.Errorf("Value %s out of range for %s", val, key)
	}

	if v, ok := getStr("dateStyle"); ok {
		if err := checkEnum("dateStyle", v, "full", "long", "medium", "short"); err != nil {
			return cfg, "RangeError", err
		}
		cfg.DateStyle = v
	}
	if v, ok := getStr("timeStyle"); ok {
		if err := checkEnum("timeStyle", v, "full", "long", "medium", "short"); err != nil {
			return cfg, "RangeError", err
		}
		cfg.TimeStyle = v
	}
	if v, ok := getStr("weekday"); ok {
		if err := checkEnum("weekday", v, "narrow", "short", "long"); err != nil {
			return cfg, "RangeError", err
		}
		cfg.Weekday = v
	}
	if v, ok := getStr("year"); ok {
		if err := checkEnum("year", v, "numeric", "2-digit"); err != nil {
			return cfg, "RangeError", err
		}
		cfg.Year = v
	}
	if v, ok := getStr("month"); ok {
		if err := checkEnum("month", v, "numeric", "2-digit", "narrow", "short", "long"); err != nil {
			return cfg, "RangeError", err
		}
		cfg.Month = v
	}
	if v, ok := getStr("day"); ok {
		if err := checkEnum("day", v, "numeric", "2-digit"); err != nil {
			return cfg, "RangeError", err
		}
		cfg.Day = v
	}
	if v, ok := getStr("hour"); ok {
		if err := checkEnum("hour", v, "numeric", "2-digit"); err != nil {
			return cfg, "RangeError", err
		}
		cfg.Hour = v
	}
	if v, ok := getStr("minute"); ok {
		if err := checkEnum("minute", v, "numeric", "2-digit"); err != nil {
			return cfg, "RangeError", err
		}
		cfg.Minute = v
	}
	if v, ok := getStr("second"); ok {
		if err := checkEnum("second", v, "numeric", "2-digit"); err != nil {
			return cfg, "RangeError", err
		}
		cfg.Second = v
	}
	if rawOpts != nil {
		if fsd, ok := rawOpts["fractionalSecondDigits"]; ok && fsd != nil {
			var n int
			switch x := fsd.(type) {
			case int64:
				n = int(x)
			case float64:
				n = int(x)
			case int:
				n = x
			}
			if n < 1 || n > 3 {
				return cfg, "RangeError", fmt.Errorf("Value %v out of range for fractionalSecondDigits", fsd)
			}
			cfg.FractionalSecondDigits = n
		}
	}
	if v, ok := getStr("timeZoneName"); ok {
		if err := checkEnum("timeZoneName", v, "short", "long", "shortOffset", "longOffset", "shortGeneric", "longGeneric"); err != nil {
			return cfg, "RangeError", err
		}
		cfg.TimeZoneName = v
	}

	hasDateTimeComponents := cfg.Weekday != "" || cfg.Year != "" || cfg.Month != "" || cfg.Day != "" ||
		cfg.Hour != "" || cfg.Minute != "" || cfg.Second != "" || cfg.FractionalSecondDigits > 0
	hasAnyComponents := hasDateTimeComponents || cfg.TimeZoneName != ""
	hasStyle := cfg.DateStyle != "" || cfg.TimeStyle != ""
	if hasStyle && hasAnyComponents {
		return cfg, "TypeError", fmt.Errorf("cannot combine dateStyle/timeStyle with individual date/time component options")
	}

	// Determine hour12 / hourCycle defaults.
	default12 := family == "en-us"
	cfg.Hour12 = default12
	if default12 {
		cfg.HourCycle = "h12"
	} else {
		cfg.HourCycle = "h23"
	}
	if v, ok := getStr("hourCycle"); ok {
		if err := checkEnum("hourCycle", v, "h11", "h12", "h23", "h24"); err != nil {
			return cfg, "RangeError", err
		}
		cfg.HourCycle = v
		cfg.Hour12 = v == "h11" || v == "h12"
	}
	if rawOpts != nil {
		if h12, ok := rawOpts["hour12"]; ok && h12 != nil {
			if b, ok := h12.(bool); ok {
				cfg.Hour12 = b
				if b {
					if cfg.HourCycle != "h11" && cfg.HourCycle != "h12" {
						cfg.HourCycle = "h12"
					}
				} else {
					if cfg.HourCycle != "h23" && cfg.HourCycle != "h24" {
						cfg.HourCycle = "h23"
					}
				}
			}
		}
	}

	// Expand dateStyle / timeStyle or default fields by mode.
	if hasStyle {
		switch cfg.DateStyle {
		case "full":
			cfg.Weekday, cfg.Year, cfg.Month, cfg.Day = "long", "numeric", "long", "numeric"
		case "long":
			cfg.Year, cfg.Month, cfg.Day = "numeric", "long", "numeric"
		case "medium":
			cfg.Year, cfg.Month, cfg.Day = "numeric", "short", "numeric"
		case "short":
			cfg.Year, cfg.Month, cfg.Day = "2-digit", "numeric", "numeric"
			if family == "sv" {
				cfg.Year, cfg.Month, cfg.Day = "numeric", "2-digit", "2-digit"
			}
		}
		switch cfg.TimeStyle {
		case "full":
			cfg.Hour, cfg.Minute, cfg.Second, cfg.TimeZoneName = defaultHourFormat(cfg), "2-digit", "2-digit", "long"
		case "long":
			cfg.Hour, cfg.Minute, cfg.Second, cfg.TimeZoneName = defaultHourFormat(cfg), "2-digit", "2-digit", "short"
		case "medium":
			cfg.Hour, cfg.Minute, cfg.Second = defaultHourFormat(cfg), "2-digit", "2-digit"
		case "short":
			cfg.Hour, cfg.Minute = defaultHourFormat(cfg), "2-digit"
		}
	} else if !hasDateTimeComponents {
		needDate := mode == "date" || mode == "datetime"
		needTime := mode == "time" || mode == "datetime"
		if needDate {
			cfg.Year = "numeric"
			if family == "sv" || family == "dmy" {
				cfg.Month, cfg.Day = "2-digit", "2-digit"
			} else {
				cfg.Month, cfg.Day = "numeric", "numeric"
			}
		}
		if needTime {
			cfg.Hour = defaultHourFormat(cfg)
			cfg.Minute = "2-digit"
			cfg.Second = "2-digit"
		}
	}

	return cfg, "", nil
}

func defaultHourFormat(cfg intlDateTimeConfig) string {
	if cfg.Hour12 {
		return "numeric"
	}
	return "2-digit"
}

func normalizeLocaleTag(v goja.Value) string {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return "en-US"
	}
	exp := v.Export()
	switch x := exp.(type) {
	case string:
		if s := strings.TrimSpace(strings.ReplaceAll(x, "_", "-")); s != "" {
			return s
		}
	case []any:
		for _, item := range x {
			if s, ok := item.(string); ok {
				if trimmed := strings.TrimSpace(strings.ReplaceAll(s, "_", "-")); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return "en-US"
}

func classifyLocaleFamily(locale string) string {
	low := strings.ToLower(locale)
	switch {
	case strings.HasPrefix(low, "zh"):
		return "zh"
	case strings.HasPrefix(low, "sv"), low == "iso", strings.HasPrefix(low, "en-ca"):
		return "sv"
	case strings.HasPrefix(low, "ja"):
		return "ja"
	case strings.HasPrefix(low, "ko"):
		return "ko"
	case strings.HasPrefix(low, "en-gb"), strings.HasPrefix(low, "en-au"),
		strings.HasPrefix(low, "de"), strings.HasPrefix(low, "fr"),
		strings.HasPrefix(low, "es"), strings.HasPrefix(low, "it"),
		strings.HasPrefix(low, "pt"), strings.HasPrefix(low, "ru"):
		return "dmy"
	default:
		return "en-us"
	}
}

func resolveTimeZone(tz string) (*time.Location, string, error) {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		return time.Local, time.Local.String(), nil
	}
	upper := strings.ToUpper(tz)
	switch upper {
	case "UTC", "GMT", "ETC/UTC", "ETC/GMT", "Z":
		return time.UTC, "UTC", nil
	}
	if loc, canon, ok := parseFixedOffsetZone(tz); ok {
		return loc, canon, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, "", fmt.Errorf("Invalid time zone specified: %s", tz)
	}
	return loc, tz, nil
}

func parseFixedOffsetZone(tz string) (*time.Location, string, bool) {
	s := strings.TrimSpace(tz)
	upper := strings.ToUpper(s)
	if strings.HasPrefix(upper, "UTC") {
		s = s[3:]
	} else if strings.HasPrefix(upper, "GMT") {
		s = s[3:]
	}
	if len(s) < 2 || (s[0] != '+' && s[0] != '-') {
		return nil, "", false
	}
	sign := 1
	if s[0] == '-' {
		sign = -1
	}
	rest := s[1:]
	var hh, mm int
	var err error
	if colon := strings.IndexByte(rest, ':'); colon >= 0 {
		hh, err = strconv.Atoi(rest[:colon])
		if err != nil {
			return nil, "", false
		}
		mm, err = strconv.Atoi(rest[colon+1:])
		if err != nil {
			return nil, "", false
		}
	} else if len(rest) == 4 {
		hh, err = strconv.Atoi(rest[:2])
		if err != nil {
			return nil, "", false
		}
		mm, err = strconv.Atoi(rest[2:])
		if err != nil {
			return nil, "", false
		}
	} else if len(rest) == 1 || len(rest) == 2 {
		hh, err = strconv.Atoi(rest)
		if err != nil {
			return nil, "", false
		}
	} else {
		return nil, "", false
	}
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return nil, "", false
	}
	signChar := "+"
	if sign < 0 {
		signChar = "-"
	}
	canon := fmt.Sprintf("%s%02d:%02d", signChar, hh, mm)
	return time.FixedZone(canon, sign*(hh*3600+mm*60)), canon, true
}

func formatIntlDateTimeParts(t time.Time, cfg intlDateTimeConfig) []intlPart {
	var parts []intlPart
	add := func(kind, val string) {
		if val != "" {
			parts = append(parts, intlPart{Type: kind, Value: val})
		}
	}

	hasDate := cfg.Weekday != "" || cfg.Year != "" || cfg.Month != "" || cfg.Day != ""
	hasTime := cfg.Hour != "" || cfg.Minute != "" || cfg.Second != "" || cfg.FractionalSecondDigits > 0

	// Date section
	if hasDate {
		useWordyMonth := cfg.Month == "long" || cfg.Month == "short" || cfg.Month == "narrow"
		switch cfg.LocaleFamily {
		case "zh":
			if useWordyMonth || cfg.DateStyle == "long" || cfg.DateStyle == "full" {
				if cfg.Year != "" {
					add("year", formatYear(t, cfg.Year))
					add("literal", "年")
				}
				if cfg.Month != "" {
					add("month", formatMonth(t, cfg.Month, cfg.LocaleFamily))
					if cfg.Month == "numeric" || cfg.Month == "2-digit" {
						add("literal", "月")
					}
				}
				if cfg.Day != "" {
					add("day", formatDay(t, cfg.Day))
					add("literal", "日")
				}
				if cfg.Weekday != "" {
					if cfg.Year != "" || cfg.Month != "" || cfg.Day != "" {
						add("literal", " ")
					}
					add("weekday", formatWeekday(t, cfg.Weekday, cfg.LocaleFamily))
				}
			} else {
				appendDelimitedDate(&parts, t, cfg, "/", []string{"year", "month", "day"})
				if cfg.Weekday != "" {
					if len(parts) > 0 {
						add("literal", " ")
					}
					add("weekday", formatWeekday(t, cfg.Weekday, cfg.LocaleFamily))
				}
			}
		case "sv":
			if cfg.Weekday != "" {
				add("weekday", formatWeekday(t, cfg.Weekday, cfg.LocaleFamily))
				if cfg.Year != "" || cfg.Month != "" || cfg.Day != "" {
					add("literal", " ")
				}
			}
			appendDelimitedDate(&parts, t, cfg, "-", []string{"year", "month", "day"})
		case "ja", "ko":
			appendDelimitedDate(&parts, t, cfg, "/", []string{"year", "month", "day"})
			if cfg.Weekday != "" {
				if len(parts) > 0 {
					add("literal", " ")
				}
				add("weekday", formatWeekday(t, cfg.Weekday, cfg.LocaleFamily))
			}
		case "dmy":
			if cfg.Weekday != "" {
				add("weekday", formatWeekday(t, cfg.Weekday, cfg.LocaleFamily))
				if cfg.Year != "" || cfg.Month != "" || cfg.Day != "" {
					add("literal", ", ")
				}
			}
			if useWordyMonth {
				if cfg.Day != "" {
					add("day", formatDay(t, cfg.Day))
					if cfg.Month != "" || cfg.Year != "" {
						add("literal", " ")
					}
				}
				if cfg.Month != "" {
					add("month", formatMonth(t, cfg.Month, cfg.LocaleFamily))
					if cfg.Year != "" {
						add("literal", " ")
					}
				}
				if cfg.Year != "" {
					add("year", formatYear(t, cfg.Year))
				}
			} else {
				appendDelimitedDate(&parts, t, cfg, "/", []string{"day", "month", "year"})
			}
		default: // en-us
			if cfg.Weekday != "" {
				add("weekday", formatWeekday(t, cfg.Weekday, cfg.LocaleFamily))
				if cfg.Year != "" || cfg.Month != "" || cfg.Day != "" {
					add("literal", ", ")
				}
			}
			if useWordyMonth {
				if cfg.Month != "" {
					add("month", formatMonth(t, cfg.Month, cfg.LocaleFamily))
					if cfg.Day != "" {
						add("literal", " ")
					} else if cfg.Year != "" {
						add("literal", " ")
					}
				}
				if cfg.Day != "" {
					add("day", formatDay(t, cfg.Day))
					if cfg.Year != "" {
						add("literal", ", ")
					}
				}
				if cfg.Year != "" {
					add("year", formatYear(t, cfg.Year))
				}
			} else {
				appendDelimitedDate(&parts, t, cfg, "/", []string{"month", "day", "year"})
			}
		}
	}

	// Separator between date and time
	if hasDate && hasTime {
		if cfg.LocaleFamily == "zh" || cfg.LocaleFamily == "sv" || cfg.LocaleFamily == "ja" || cfg.LocaleFamily == "ko" {
			add("literal", " ")
		} else {
			add("literal", ", ")
		}
	}

	// Time section
	if hasTime {
		dayPeriod := ""
		if cfg.Hour12 && cfg.Hour != "" {
			if cfg.LocaleFamily == "zh" {
				if t.Hour() < 12 {
					dayPeriod = "上午"
				} else {
					dayPeriod = "下午"
				}
				add("dayPeriod", dayPeriod)
			} else {
				if t.Hour() < 12 {
					dayPeriod = "AM"
				} else {
					dayPeriod = "PM"
				}
			}
		}

		needColon := false
		if cfg.Hour != "" {
			add("hour", formatHour(t, cfg.Hour, cfg.HourCycle))
			needColon = true
		}
		if cfg.Minute != "" {
			if needColon {
				add("literal", ":")
			}
			add("minute", formatTwoOrNumeric(t.Minute(), cfg.Minute, needColon))
			needColon = true
		}
		if cfg.Second != "" {
			if needColon {
				add("literal", ":")
			}
			add("second", formatTwoOrNumeric(t.Second(), cfg.Second, needColon))
		}
		if cfg.FractionalSecondDigits > 0 {
			ms := t.Nanosecond() / 1_000_000
			frac := fmt.Sprintf("%03d", ms)[:cfg.FractionalSecondDigits]
			add("literal", ".")
			add("fractionalSecond", frac)
		}
		if dayPeriod != "" && cfg.LocaleFamily != "zh" {
			add("literal", " ")
			add("dayPeriod", dayPeriod)
		}
	}

	// Time zone name
	if cfg.TimeZoneName != "" {
		if len(parts) > 0 {
			add("literal", " ")
		}
		add("timeZoneName", formatTimeZoneName(t, cfg.TimeZoneName, cfg.TimeZone))
	}

	return parts
}

func appendDelimitedDate(parts *[]intlPart, t time.Time, cfg intlDateTimeConfig, sep string, order []string) {
	first := true
	for _, field := range order {
		var val string
		switch field {
		case "year":
			if cfg.Year != "" {
				val = formatYear(t, cfg.Year)
			}
		case "month":
			if cfg.Month != "" {
				val = formatMonth(t, cfg.Month, cfg.LocaleFamily)
			}
		case "day":
			if cfg.Day != "" {
				val = formatDay(t, cfg.Day)
			}
		}
		if val == "" {
			continue
		}
		if !first {
			*parts = append(*parts, intlPart{Type: "literal", Value: sep})
		}
		*parts = append(*parts, intlPart{Type: field, Value: val})
		first = false
	}
}

func formatYear(t time.Time, style string) string {
	if style == "2-digit" {
		return fmt.Sprintf("%02d", t.Year()%100)
	}
	return fmt.Sprintf("%04d", t.Year())
}

func formatMonth(t time.Time, style, family string) string {
	m := int(t.Month())
	switch style {
	case "2-digit":
		return fmt.Sprintf("%02d", m)
	case "numeric":
		return strconv.Itoa(m)
	case "long":
		if family == "zh" {
			return fmt.Sprintf("%d月", m)
		}
		return enMonthsLong[m-1]
	case "short":
		if family == "zh" {
			return fmt.Sprintf("%d月", m)
		}
		return enMonthsShort[m-1]
	case "narrow":
		if family == "zh" {
			return strconv.Itoa(m)
		}
		return enMonthsNarrow[m-1]
	default:
		return strconv.Itoa(m)
	}
}

func formatDay(t time.Time, style string) string {
	if style == "2-digit" {
		return fmt.Sprintf("%02d", t.Day())
	}
	return strconv.Itoa(t.Day())
}

func formatWeekday(t time.Time, style, family string) string {
	w := int(t.Weekday())
	if family == "zh" {
		switch style {
		case "short":
			return zhWeekdaysShort[w]
		case "narrow":
			return zhWeekdaysNarrow[w]
		default:
			return zhWeekdaysLong[w]
		}
	}
	switch style {
	case "short":
		return enWeekdaysShort[w]
	case "narrow":
		return enWeekdaysNarrow[w]
	default:
		return enWeekdaysLong[w]
	}
}

func formatHour(t time.Time, style, hourCycle string) string {
	h := t.Hour()
	switch hourCycle {
	case "h11":
		h = h % 12
	case "h12":
		h = h % 12
		if h == 0 {
			h = 12
		}
	case "h24":
		if h == 0 {
			h = 24
		}
	default: // h23
	}
	if style == "2-digit" {
		return fmt.Sprintf("%02d", h)
	}
	return strconv.Itoa(h)
}

func formatTwoOrNumeric(n int, style string, forcePad bool) string {
	if style == "2-digit" || forcePad {
		return fmt.Sprintf("%02d", n)
	}
	return strconv.Itoa(n)
}

func formatTimeZoneName(t time.Time, style, tzName string) string {
	abbr, offsetSec := t.Zone()
	sign := "+"
	abs := offsetSec
	if abs < 0 {
		sign = "-"
		abs = -abs
	}
	hh := abs / 3600
	mm := (abs % 3600) / 60

	shortOffset := "GMT"
	if offsetSec != 0 {
		if mm == 0 {
			shortOffset = fmt.Sprintf("GMT%s%d", sign, hh)
		} else {
			shortOffset = fmt.Sprintf("GMT%s%d:%02d", sign, hh, mm)
		}
	}
	longOffset := fmt.Sprintf("GMT%s%02d:%02d", sign, hh, mm)

	switch style {
	case "shortOffset":
		return shortOffset
	case "longOffset":
		return longOffset
	case "short":
		if abbr != "" && abbr[0] != '+' && abbr[0] != '-' {
			return abbr
		}
		return shortOffset
	case "long", "longGeneric", "shortGeneric":
		if tzName != "" && tzName != "Local" {
			return tzName
		}
		return longOffset
	default:
		return shortOffset
	}
}
