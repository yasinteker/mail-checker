package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func rcpt(addr string) recipient { return recipient{EmailAddress: emailAddress{Address: addr}} }

var me = identity{Name: "Test Kullanici", Email: "me@firma.com"}

func TestMatchAddr(t *testing.T) {
	list := splitList(" Patron@Firma.com , musteri.com.tr ")
	cases := map[string]bool{
		"patron@firma.com":          true,
		"PATRON@firma.com":          true,
		"baska@firma.com":           false,
		"ali@musteri.com.tr":        true,
		"ali@destek.musteri.com.tr": true,
		"ali@kotumusteri.com.tr":    false,
	}
	for addr, want := range cases {
		if got := matchAddr(addr, list); got != want {
			t.Errorf("%s: %v, beklenen %v", addr, got, want)
		}
	}
}

func TestKeywordTurkishCase(t *testing.T) {
	r := rules{Keywords: splitList("acil,çöktü,ödeme alınamıyor")}
	for _, s := range []string{"ACİL: sunucu", "Site ÇÖKTÜ", "Ödeme Alınamıyor!"} {
		if r.keywordHit(s) == "" {
			t.Errorf("%q eslesmeli", s)
		}
	}
	if r.keywordHit("Haftalık rapor") != "" {
		t.Error("eslesmemeli")
	}
}

func TestCleanBodyStripsQuotes(t *testing.T) {
	body := "Merhaba,\r\n\r\n\r\n\r\nYarın canlıya çıkıyoruz.\r\n\r\nFrom: Eski Kişi <x@y.com>\r\nSent: dün\r\nEski içerik"
	got := cleanBody(body)
	if strings.Contains(got, "Eski içerik") || !strings.Contains(got, "canlıya") {
		t.Fatalf("yanit zinciri kirpilmadi: %q", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Fatal("bos satirlar sadelesmedi")
	}
	tr := "Selam\n\n18 Kas 2026 Çar, 16:29 tarihinde şunu yazdı:\n> eski"
	if strings.Contains(cleanBody(tr), "eski") {
		t.Fatal("Turkce alinti kirpilmadi")
	}
	long := strings.Repeat("ş", maxBodyRunes+50)
	if c := cleanBody(long); !strings.HasSuffix(c, "[...kırpıldı]") {
		t.Fatal("uzun govde kirpilmadi")
	}
}

func TestParseVerdict(t *testing.T) {
	v, err := parseVerdict("```json\n{\"level\":\"urgent\",\"relevant\":true,\"reason\":\"Canlıda ödeme hatası\"}\n```")
	if err != nil || v.Level != levelUrgent || !v.Relevant {
		t.Fatalf("%+v %v", v, err)
	}
	if _, err := parseVerdict(`{"level":"panic"}`); err == nil {
		t.Fatal("gecersiz seviye kabul edildi")
	}
	if _, err := parseVerdict("yok"); err == nil {
		t.Fatal("JSON'suz yanit kabul edildi")
	}
}

func TestDecide(t *testing.T) {
	r := rules{VIP: []string{"patron@firma.com"}, Ignore: []string{"noreply.com"}, Keywords: []string{"acil"}}
	base := message{From: recipient{EmailAddress: emailAddress{Address: "biri@dis.com"}},
		ToRecipients: []recipient{rcpt("me@firma.com"), rcpt("diger@firma.com")}, Subject: "Bilgi"}

	// Modelin "onemsiz ve ilgisiz" karari -> okundu isaretlenir
	d := decide(base, me, r, verdict{Level: levelNone, Relevant: false}, nil)
	if d.Level != levelNone || !d.MarkRead {
		t.Fatalf("ilgisiz: %+v", d)
	}
	// Ilgili ama onemsiz -> okundu yapilmaz
	if d := decide(base, me, r, verdict{Level: levelNone, Relevant: true}, nil); d.MarkRead {
		t.Fatal("ilgili mail okundu yapildi")
	}
	// Is arkadasinin yalnizca bana yazdigi -> asla okundu yapilmaz
	solo := base
	solo.From.EmailAddress.Address = "kolega@firma.com"
	solo.ToRecipients = []recipient{rcpt("ME@firma.com")}
	if d := decide(solo, me, r, verdict{Level: levelNone}, nil); d.MarkRead {
		t.Fatal("is arkadasinin yalnizca bana yazdigi okundu yapildi")
	}
	// Dis gondericiden yalnizca bana gelen bulten -> okundu yapilabilir
	news := base
	news.From.EmailAddress.Address = "kampanya@pazaryeri.com"
	news.ToRecipients = []recipient{rcpt("me@firma.com")}
	if d := decide(news, me, r, verdict{Level: levelNone}, nil); !d.MarkRead {
		t.Fatal("bulten okundu yapilmadi")
	}
	// VIP -> en az onemli, okundu yapilmaz
	vip := base
	vip.From.EmailAddress.Address = "patron@firma.com"
	if d := decide(vip, me, r, verdict{Level: levelNone}, nil); d.Level != levelImportant || d.MarkRead {
		t.Fatalf("vip: %+v", d)
	}
	// Anahtar kelime + model hatasi -> kurala gore onemli
	kw := base
	kw.Subject = "ACİL bakar mısın"
	if d := decide(kw, me, r, verdict{}, errors.New("zaman asimi")); d.Level != levelImportant || d.MarkRead {
		t.Fatalf("model hatasi: %+v", d)
	}
	// Model hatasi, kural yok -> bildirim yok, okundu da yok
	if d := decide(base, me, r, verdict{}, errors.New("x")); d.Level != levelNone || d.MarkRead {
		t.Fatalf("model hatasi kuralsiz: %+v", d)
	}
	// Model acil derse acil
	if d := decide(base, me, r, verdict{Level: levelUrgent, Relevant: true}, nil); d.Level != levelUrgent {
		t.Fatal("acil kaybedildi")
	}
	// Yoksayilan gonderici
	ign := base
	ign.From.EmailAddress.Address = "rapor@noreply.com"
	if d := decide(ign, me, r, verdict{Level: levelUrgent}, nil); d.Level != levelNone || !d.MarkRead {
		t.Fatalf("yoksay: %+v", d)
	}
}

func TestBuildPromptMarksUser(t *testing.T) {
	m := message{From: recipient{EmailAddress: emailAddress{Name: "Ali", Address: "ali@dis.com"}},
		CcRecipients: []recipient{rcpt("me@firma.com")}, Subject: "Konu", Importance: "high"}
	m.Body.Content = "Gövde"
	p := buildPrompt(m, me, []string{"ipucu"})
	for _, want := range []string{"Bilgi (CC)", "KULLANICI", "yüksek önemli", "<email>", "Kural ipuçları: ipucu"} {
		if !strings.Contains(p, want) {
			t.Errorf("istemde %q yok", want)
		}
	}
}

func TestAddressedUrgent(t *testing.T) {
	me := identity{Name: "Yasin Teker", Email: "yasin.teker@firma.com"}
	r := rules{AddressedUrgent: []string{"support@bestcloudfor.me"}}
	m := message{From: recipient{EmailAddress: emailAddress{Address: "support@bestcloudfor.me"}},
		ToRecipients: []recipient{rcpt("support@firma.com")}, Subject: "Re: test ortamı [#610815]"}
	m.Body.Content = "-- reply above this line --\n\nDear Yasin Teker,\nSelamlar, ortam hazır."
	if d := decide(m, me, r, verdict{Level: levelNone, Relevant: false}, nil); d.Level != levelUrgent || d.MarkRead {
		t.Fatalf("bana hitaben ticket acil olmali: %+v", d)
	}
	if d := decide(m, me, r, verdict{}, errors.New("x")); d.Level != levelUrgent {
		t.Fatal("model hatasinda da acil olmali")
	}
	other := m
	other.Body.Content = "-- reply above this line --\n\nDear Ali Bulut,\nTalep kapatıldı."
	if d := decide(other, me, r, verdict{Level: levelNone}, nil); d.Level == levelUrgent {
		t.Fatal("baskasina hitaben ticket acil olmamali")
	}
	tr := other
	tr.Body.Content = "YASİN BEY merhaba, dönüşünüzü bekliyoruz"
	if !addressedToMe(tr, me) {
		t.Fatal("Turkce hitap taninmadi")
	}
}

func TestDecisionSource(t *testing.T) {
	r := rules{VIP: []string{"patron@firma.com"}, Keywords: []string{"acil"}}
	m := message{From: recipient{EmailAddress: emailAddress{Address: "patron@firma.com"}}, Subject: "Acil toplantı"}
	if d := decide(m, me, r, verdict{Level: levelUrgent}, nil); d.Source != "kural:vip,anahtar:acil+yz" {
		t.Fatalf("kaynak: %q", d.Source)
	}
	if d := decide(m, me, r, verdict{Level: levelNone}, nil); d.Source != "kural:vip,anahtar:acil (yz: none)" {
		t.Fatalf("kaynak: %q", d.Source)
	}
	plain := message{From: recipient{EmailAddress: emailAddress{Address: "x@y.com"}}, Subject: "Bilgi"}
	if d := decide(plain, me, r, verdict{Level: levelNone}, errors.New("zaman")); !strings.Contains(d.Source, "yz hatası") {
		t.Fatalf("kaynak: %q", d.Source)
	}
}

func TestRepeatKey(t *testing.T) {
	a := repeatKey("Alert@x.com", "Failed Payment Requests - 2026-11-19 10:05")
	b := repeatKey("alert@x.com", "RE: Failed Payment Requests - 2026-11-19 11:45")
	if a != b {
		t.Fatalf("%q != %q", a, b)
	}
	if repeatKey("alert@x.com", "Başka konu") == a {
		t.Fatal("farkli konular ayni anahtar")
	}
}

func TestReceivedLine(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Istanbul")
	now := time.Date(2026, 11, 20, 6, 13, 0, 0, time.UTC) // 09:13 TR
	cases := []struct {
		rec  time.Time
		want string
	}{
		{now.Add(-time.Minute), ""},
		{time.Date(2026, 11, 20, 3, 40, 0, 0, time.UTC), "Geliş: 06:40"},
		{time.Date(2026, 11, 19, 13, 50, 0, 0, time.UTC), "Geliş: 19.11 16:50"},
		{time.Time{}, ""},
	}
	for _, c := range cases {
		if got := receivedLine(c.rec, now, loc); got != c.want {
			t.Errorf("receivedLine(%v) = %q, want %q", c.rec, got, c.want)
		}
	}
}
