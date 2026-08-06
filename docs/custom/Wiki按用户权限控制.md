# Wiki 按用户权限控制

Wiki 的生成、浏览和后端处理能力保持不变，但“新开启 Wiki 索引”采用按用户显式授权：

- 默认用户没有 Wiki 选择权，知识库编辑器中的 Wiki 开关显示为灰色。
- 鼠标悬停灰色开关时提示“Wiki功能暂时无法使用，请联系管理员开放wiki权限”。
- 系统管理员可进入“系统设置 → Wiki 权限”，通过防抖搜索和服务端分页查找用户并单独开放或关闭权限，不一次性加载全部用户。
- 权限关闭不会删除、停用或改变用户已有的 Wiki 知识库，只阻止创建时选择 Wiki，或把既有非 Wiki 知识库切换为 Wiki。

## API

- `GET /api/v1/custom/wiki-access/me`：返回当前用户是否可选择 Wiki。
- `GET /api/v1/custom/wiki-access/users?q=&page=&page_size=`：系统管理员分页查询用户与 Wiki 权限，响应包含用户列表、总数、页码和每页数量。
- `PUT /api/v1/custom/wiki-access/users/:user_id`：系统管理员更新用户权限，请求体为 `{"wiki_enabled": true|false}`。

权限保存在 `custom_wiki_user_permissions`。不存在记录等同于关闭；后端在知识库创建和从非 Wiki 切换到 Wiki 时再次校验，不能通过绕过前端直接开启。
