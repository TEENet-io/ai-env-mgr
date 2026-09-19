# 数据库与迁移

表结构在 `migrations/*.sql`,以 `go:embed` 编进二进制。控制台启动时自己把待执行的迁移跑完,`cmd/migrate` 用于首次初始化、回滚和查看。

## 命名与规则

- 文件名 `<版本>_<名字>.up.sql` / `.down.sql`,版本从 0001 递增,每个版本必须有 down。
- **已发布的迁移文件不能再改**:运行器记录每个文件的 sha256,改过就拒绝启动(`ErrChecksumMismatch`)。要改表就加新版本。
- 每个迁移在自己的事务里执行,PostgreSQL 的 DDL 可回滚,所以中途失败不会留下半套表。
- 两个进程同时迁移时靠 `pg_advisory_lock` 排队,拿到锁后重新计算待执行清单。
- 数据库里有本二进制不认识的迁移版本时拒绝启动:那说明新版本迁过库又回滚了代码,继续跑只会在某个页面上报"列不存在"。

## 数据库账号

| 账号 | 用途 | 权限 |
|---|---|---|
| 实例的特权账号(本机 `postgres`,RDS 上是高权限账号) | 只跑迁移:`cmd/migrate up` | 建表、建索引、授权 |
| `aienv_app` | 控制台 + Worker | 业务表增删改查;`audit_events` **只能 INSERT/SELECT**;`task_attempts` 可 INSERT/UPDATE 不可 DELETE(领任务时开一行,结束时写结果);`schema_migrations` **只读** |
| `aienv_ro` | 查询、报表、排障 | 只 SELECT |

授权网格在 `grants.sql`,**由迁移运行器在每批迁移之后重放**(也可手动 `migrate grants`)。它不是迁移:迁移只跑一次,而一次性的授权覆盖不到以后新建的表——原先 `0002` 里那种写法就是这么漏的。

角色以 `NOLOGIN` 无密码建出,可以安全入库、反复执行。密码在实例上单独设一次:

```sql
alter role aienv_app login password '<从秘密存储取>';
alter role aienv_ro  login password '<...>';
```

控制台用 `aienv_app` 连接;启动时它只读迁移记录,发现有待执行的迁移会拒绝启动并提示先以特权账号跑 `migrate up`。

## 本地开发库

RDS 到位之前用本机 Docker 起一个同版本的库,连接串换掉即可:

```sh
docker run -d --name aienv-pg15 \
  -e POSTGRES_PASSWORD=devpass -e POSTGRES_DB=aienv \
  -p 127.0.0.1:5433:5432 --memory 512m --restart unless-stopped \
  postgres:15-alpine

export AIENVMGR_DB_DSN='postgres://postgres:devpass@127.0.0.1:5433/aienv?sslmode=disable'
go run ./cmd/migrate status
go run ./cmd/migrate up
```

只监听 `127.0.0.1`,不对外。生产的 RDS 只开私网,由同 VPC 的 ECS 连接。

## 跑集成测试

`internal/dbstore` 的测试没有 `TEST_PG_DSN` 就跳过(所以 `make check` 在没有数据库的机器上照样过)。给了就会 **先 drop 掉该库的 public schema**,所以只能指向专门的测试库:

```sh
docker exec aienv-pg15 createdb -U postgres aienv_test
TEST_PG_DSN='postgres://postgres:devpass@127.0.0.1:5433/aienv_test?sslmode=disable' \
  go test ./internal/dbstore/
```

## 连接串从哪来

优先环境变量 `AIENVMGR_DB_DSN`(systemd 的 `EnvironmentFile`,权限 600)。命令行 `-dsn` 只在临时排障时用:连接串带密码,写在命令行上就进了进程列表和 shell 历史。
