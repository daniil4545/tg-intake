-- +goose Up
-- Контракт готовности сжат до ядра, и появился тип mixed: баг и пожелание в
-- одном обращении (docs/specs/ticket-form.md, Р-1, Р-14).
ALTER TABLE cases DROP CONSTRAINT cases_kind_check,
                  ADD  CONSTRAINT cases_kind_check
                  CHECK (kind IN ('bug', 'feature', 'question', 'mixed'));

-- Обращения в полёте переводятся на ключи ядра: expected и actual - в wrong,
-- result - в need, problem - в why, прочие отбрасываются (их слова остались в
-- протоколе и журнале). Новый ключ старым не перезаписывается, поэтому
-- повторный up после отката ничего не портит. publishing тоже: работа
-- публикации может ждать выката, а тело тикета строится по ключам ядра.
-- updated_at не трогается: это не действие автора.
WITH mapped AS (
    SELECT id, gaps AS old_gaps, jsonb_strip_nulls(jsonb_build_object(
        'case',     contract -> 'case',
        'question', contract -> 'question',
        'wrong',    COALESCE(contract -> 'wrong',
                        CASE WHEN gaps ?| ARRAY['expected', 'actual'] THEN NULL
                        ELSE to_jsonb(NULLIF(concat_ws(E'\n', NULLIF(contract ->> 'expected', ''),
                                                              NULLIF(contract ->> 'actual', '')), ''))
                        END),
        'need',     COALESCE(contract -> 'need', contract -> 'result'),
        'why',      COALESCE(contract -> 'why', contract -> 'problem'))) AS contract
    FROM cases
    WHERE status IN ('interview', 'summary', 'publishing')
)
UPDATE cases SET
    contract = mapped.contract,
    gaps = (
        SELECT COALESCE(jsonb_agg(DISTINCT key), '[]')
        FROM (SELECT CASE old WHEN 'expected' THEN 'wrong' WHEN 'actual' THEN 'wrong'
                              WHEN 'result' THEN 'need' WHEN 'problem' THEN 'why'
                              ELSE old END AS key
              FROM jsonb_array_elements_text(mapped.old_gaps) AS old) AS keys
        WHERE key IN ('case', 'question', 'wrong', 'need', 'why')
          AND NOT (mapped.contract ? key))
FROM mapped
WHERE cases.id = mapped.id;

-- Метка неполноты считается по ядру: у пожелания раньше были обязательны и
-- today, и done, и без пересчёта метка ушла бы без строки «Не уточнено».
UPDATE cases SET incomplete = gaps <> '[]'
WHERE status IN ('summary', 'publishing');

-- +goose Down
-- Старые ключи не восстанавливаются: старый контракт переспросит пункты. Новые
-- снимаются, чтобы gaps не держал ключей, которых старый контракт не знает.
UPDATE cases SET kind = 'bug' WHERE kind = 'mixed';

UPDATE cases SET contract = contract - 'wrong' - 'need' - 'why',
                 gaps     = gaps - 'wrong' - 'need' - 'why'
WHERE status IN ('interview', 'summary', 'publishing');

ALTER TABLE cases DROP CONSTRAINT cases_kind_check,
                  ADD  CONSTRAINT cases_kind_check
                  CHECK (kind IN ('bug', 'feature', 'question'));
