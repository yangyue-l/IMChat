-- 存量库结构对齐：索引重建、唯一约束上线、无用索引清理
--
-- 表结构的唯一事实来源仍是 internal/model 的 gorm tag：新库启动时 AutoMigrate 一次建好。
-- 但这三件事 AutoMigrate 做不到，必须用 SQL 补：
--   1. 索引换了列顺序 / 换了唯一性 -> 同名旧索引会让 AutoMigrate 直接跳过，得先删；
--   2. 唯一索引上线前的数据清洗 -> 空串、重复值会让建索引直接失败；
--   3. 删除索引 -> AutoMigrate 只会新增，永远不会删。
--
-- 执行顺序：先跑本脚本，再重启服务让 AutoMigrate 把新索引建回来。
-- 用法：mysql -h127.0.0.1 -P13306 -uroot -p123456 imchat < deploy/migrate/20261024-index-refactor.sql
-- 脚本可重复执行；messages 索引重建期间缺一个分页索引，安排在低峰期。
--
-- 本脚本对应的模型改动：
--   messages.(conv_id, seq)        idx_conv_seq -> uk_conv_seq（普通索引改唯一）
--   conversations.idx_user_last    补上 user_id 首列
--   users.phone                    string + 普通索引 -> *string + uk_users_phone（唯一）
--   所有 BaseModel 表              去掉用不到的 created_at 单列索引

SET NAMES utf8mb4;

-- ---------------------------------------------------------------------------
-- 1. 唯一索引上线前的数据准备
-- ---------------------------------------------------------------------------

-- phone 未绑定的旧数据是空串，唯一索引下多个 NULL 合法、多个 '' 直接建失败
UPDATE users SET phone = NULL WHERE phone = '';

-- (conv_id, seq) 加唯一约束前先确认存量无重复。
-- 有结果说明历史上 seq 曾经重叠过：脚本不会替你删数据，先人工决定保留哪条，
-- 否则 AutoMigrate 建 uk_conv_seq 时会报 1062 并导致服务启动失败。
SELECT conv_id, seq, COUNT(*) AS cnt, GROUP_CONCAT(id) AS msg_ids
FROM messages
GROUP BY conv_id, seq
HAVING cnt > 1;

-- ---------------------------------------------------------------------------
-- 2. 删除待重建 / 待废弃的索引
-- ---------------------------------------------------------------------------

-- MySQL 8 没有 DROP INDEX IF EXISTS，用存储过程包一层，保证脚本可重复执行
DROP PROCEDURE IF EXISTS imchat_drop_index;
DELIMITER $$
CREATE PROCEDURE imchat_drop_index(IN tbl VARCHAR(64), IN idx VARCHAR(64))
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.statistics
               WHERE table_schema = DATABASE() AND table_name = tbl AND index_name = idx) THEN
        SET @ddl = CONCAT('ALTER TABLE `', tbl, '` DROP INDEX `', idx, '`');
        PREPARE stmt FROM @ddl;
        EXECUTE stmt;
        DEALLOCATE PREPARE stmt;
    END IF;
END$$
DELIMITER ;

-- 2.1 换了唯一性的索引：删掉后由 AutoMigrate 重建为 uk_conv_seq
CALL imchat_drop_index('messages', 'idx_conv_seq');

-- 2.2 缺列 / 列序不对的索引：删掉后由 AutoMigrate 按 tag 重建
CALL imchat_drop_index('conversations', 'idx_user_last');
CALL imchat_drop_index('users', 'idx_users_phone'); -- 由 uk_users_phone 取代

-- 2.3 没有任何查询会走到的 created_at 单列索引（12 张表各一个）
CALL imchat_drop_index('users', 'idx_users_created_at');
CALL imchat_drop_index('friends', 'idx_friends_created_at');
CALL imchat_drop_index('friend_requests', 'idx_friend_requests_created_at');
CALL imchat_drop_index('blacklists', 'idx_blacklists_created_at');
CALL imchat_drop_index('groups', 'idx_groups_created_at');
CALL imchat_drop_index('group_members', 'idx_group_members_created_at');
CALL imchat_drop_index('messages', 'idx_messages_created_at');
CALL imchat_drop_index('conversations', 'idx_conversations_created_at');
CALL imchat_drop_index('conversation_reads', 'idx_conversation_reads_created_at');
CALL imchat_drop_index('moments', 'idx_moments_created_at');
CALL imchat_drop_index('moment_likes', 'idx_moment_likes_created_at');
CALL imchat_drop_index('moment_comments', 'idx_moment_comments_created_at');

DROP PROCEDURE imchat_drop_index;

-- ---------------------------------------------------------------------------
-- 3. 重启服务后应存在的结果（用来自检）
-- ---------------------------------------------------------------------------
-- SHOW INDEX FROM messages;            -- 应有 uk_conv_seq(conv_id, seq) UNIQUE
-- SHOW INDEX FROM conversations;       -- idx_user_last 首列必须是 user_id
-- SHOW INDEX FROM users;               -- 应有 uk_users_phone UNIQUE，且 phone 列 NULLABLE
