-- Иконка категории — эмодзи, пустая строка значит «без иконки». Стартовым
-- категориям, которые уже есть у пользователей, ставим иконки по названию.
ALTER TABLE categories ADD COLUMN icon TEXT NOT NULL DEFAULT '';

UPDATE categories SET icon = CASE name
    WHEN 'Продукты' THEN '🛒'
    WHEN 'Кафе и доставка' THEN '🍕'
    WHEN 'Транспорт' THEN '🚗'
    WHEN 'Жильё и связь' THEN '🏠'
    WHEN 'Здоровье' THEN '💊'
    WHEN 'Подписки' THEN '📺'
    WHEN 'Спорт' THEN '🏋️'
    WHEN 'Одежда' THEN '👕'
    WHEN 'Развлечения' THEN '🎬'
    WHEN 'Накопления' THEN '🐷'
    WHEN 'Зарплата' THEN '💰'
    WHEN 'Подработка' THEN '💼'
    WHEN 'Проценты по вкладу' THEN '📈'
    WHEN 'Возврат' THEN '↩️'
    ELSE icon END
WHERE icon = '';
