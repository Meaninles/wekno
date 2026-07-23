# 批量所有权验收十二号

唯一标记：`WEKNORA-WIKI-E2E-20260721-BULK-OWNER-12-B2`

十二号样本验证核心解析任务具有稳定 owner。只有租户、知识库、文档、处理代际和 owner 全部匹配的任务，才允许提交分块及索引；终态时 owner 必须清空。

重复投递必须幂等，不能创建重复产物，也不能把 completed 文档回退到 processing 或 failed。
