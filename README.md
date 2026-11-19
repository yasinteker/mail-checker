# mail-checker

Outlook (Microsoft 365) posta kutusunun tüm klasörlerini (gönderilmiş, taslak, silinmiş, gereksiz ve giden hariç) izler; her yeni maili kurallar + Claude (Haiku) ile değerlendirir:

- **Acil** → telefona yüksek öncelikli bildirim (`fcm-bridge`, severity `critical`)
- **Önemli** → normal bildirim (severity `info`)
- **Önemsiz ve kullanıcıyla ilgisiz** → (açıksa) okundu işaretlenir
- Her gün `DIGEST_HOUR`'da kısa özet bildirimi

Mail gövdesi saklanmaz. Veritabanında mail kimliğinin özeti (sha256), seviye ve tarih 30 gün;
gönderen, konu, gerekçe cümlesi ve kararın dayanağı (`source`: `yz`, `kural:vip+yz`, `kural:hitap`, …) **7 gün** tutulur.
Microsoft yenileme anahtarı AES-GCM ile şifreli saklanır.

## Akış

1. Dakikada bir (`POLL_INTERVAL`) Graph API'den son taramadan beri gelen mailleri çeker (10 dk örtüşme, kimlikle tekilleştirme).
2. Aynı göndericiden aynı konuyla (rakamlar yok sayılarak) gelen tekrarlar için saatte en fazla bir bildirim gider.
3. Zaten okunmuş mailler ve kullanıcının kendi mailleri değerlendirilmez.
3. Kurallar: `ADDRESSED_URGENT_SENDERS` + kullanıcıya hitap → her zaman acil. `VIP_SENDERS` ve konu satırında `URGENT_KEYWORDS` → en az "önemli". `IGNORE_SENDERS` → bildirim yok.
4. Claude seviyeyi (none/important/urgent), ilgiyi ve tek cümlelik nedeni döndürür. Model yanıt vermezse yalnızca kurallar uygulanır.
5. Okundu işaretleme yalnızca: seviye none + model "ilgisiz" dedi + VIP değil + iş arkadaşının yalnızca kullanıcıya yazdığı bir mail değil.

Microsoft girişi düşerse servis cihaz kodunu telefona bildirim olarak gönderir (`microsoft.com/device` + kod).

## Ortam değişkenleri

| Değişken | Varsayılan | Açıklama |
|---|---|---|
| `DATABASE_URL` | — | PostgreSQL |
| `MS_TENANT_ID`, `MS_CLIENT_ID` | — | Entra uygulama kaydı (public client, delegated izinler) |
| `ANTHROPIC_API_KEY` (veya `CLAUDE_API`) | — | Claude API anahtarı |
| `CLAUDE_MODEL` | `claude-haiku-5-5` | |
| `FCM_NOTIFY_URL`, `FCM_BRIDGE_API_KEY` | — | Bildirim köprüsü |
| `TOKEN_ENC_KEY` | — | 32 bayt, base64 (`openssl rand -base64 32`) |
| `MARK_READ_IRRELEVANT` | `false` | `true`: ilgisiz önemsiz mailleri okundu yap (Mail.ReadWrite izni ister) |
| `VIP_SENDERS` | | Virgülle: `ad@firma.com` ya da `musteri.com` (alt alan adları dahil) |
| `IGNORE_SENDERS` | | Aynı biçim; yapay zekaya sorulmaz, bildirim yok, okundu yapılabilir (otomatik rapor göndericileri) |
| `ADDRESSED_URGENT_SENDERS` | | Aynı biçim; bu göndericilerin kullanıcıya hitaben (Kime alanında ya da "Dear Ad Soyad", "Ad Bey" …) mailleri her zaman acil |
| `URGENT_KEYWORDS` | acil, kesinti, çöktü, … | Konu satırı anahtar kelimeleri |
| `POLL_INTERVAL` | `60s` | En az 15s |
| `DIGEST_HOUR` | `18` | Günlük özet saati, `-1` kapalı |
| `TZ_NAME` | `Europe/Istanbul` | |
| `DRY_RUN` | `false` | Bildirim göndermez, okundu yapmaz; yalnızca loglar |
| `MS_BOOTSTRAP_REFRESH_TOKEN` | | Tek seferlik: veritabanında anahtar yoksa içeri alınır, sonra silin |

## Değerlendirme (ayar yaparken)

```sh
MS_ACCESS_TOKEN=... ANTHROPIC_API_KEY=... go run . -eval 20
```

Son 20 maili değerlendirip sonucu yazar; bildirim göndermez, hiçbir maili değiştirmez.

## Sağlık

`GET /health` → son başarılı tarama 5 aralıktan eskiyse `503`. Docker `HEALTHCHECK` bunu kullanır.

## Kararları inceleme

```sql
SELECT processed_at AT TIME ZONE 'Europe/Istanbul' AS zaman, level, notified, marked_read, source, sender, subject, reason
FROM processed WHERE sender IS NOT NULL ORDER BY processed_at DESC LIMIT 50;
```
