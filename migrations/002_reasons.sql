-- Karar ayrintilari (gonderen, konu, gerekce, karar kaynagi) yalnizca 7 gun tutulur; sonra
-- bosaltilir (store.forgetDetails). Satirin kendisi (ozet kimlik + seviye) 30 gun kalir.
ALTER TABLE processed ADD COLUMN IF NOT EXISTS sender  text;
ALTER TABLE processed ADD COLUMN IF NOT EXISTS subject text;
ALTER TABLE processed ADD COLUMN IF NOT EXISTS reason  text;
ALTER TABLE processed ADD COLUMN IF NOT EXISTS source  text;
