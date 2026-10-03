-- Название бекапа, которое задаёт пользователь; пустое — в списке показывается дата.
ALTER TABLE backups ADD COLUMN name TEXT NOT NULL DEFAULT '';
