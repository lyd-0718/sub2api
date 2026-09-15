-- 240: kimi 并发上限统一下调为 3（存量）。
--
-- 需求方拍板：并发上限统一 3。accounts.concurrency 的建表默认值本来就是 3
-- （001_init.sql），存量 >3 的 kimi 账号来自早期前端新建表单初值 10。
-- 只下调 >3 的账号：人工设过 1/2 的保持不动（它们比 3 更保守，不应被抬高）。
--
-- capability 上限（cap）另由 account_concurrency_caps 治理，本迁移只收敛配置并发。
UPDATE accounts
   SET concurrency = 3
 WHERE platform = 'kimi'
   AND concurrency > 3
   AND deleted_at IS NULL;
