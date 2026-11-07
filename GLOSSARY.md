# torrentfs

torrentfs 把 BitTorrent 内容按需求取到内存缓存，并投影成一棵只读文件树，通过 FUSE、内置 SMB 或签名 HTTP 直链对外提供。本文件只收录项目特有的领域词汇；通用编程概念不在此列。

## 配置

**配置项** (Config Key)：
一个用点路径命名的独立设置叶子，例如 `http.auth.token_ttl`；它是用户可以单独赋值的最小单位。
_Avoid_: 字段、参数、设置项

**配置文件** (Config File)：
一份由用户显式提供的 TOML 文件，承载部分或全部配置项。
_Avoid_: 系统配置、settings

**配置来源** (Config Source)：
为配置项提供取值的一类输入：内置默认值、配置文件或环境变量；命令行参数只用来指定配置文件路径，不直接提供配置项的值。多个来源按固定优先级合并成生效配置。
_Avoid_: 配置层、来源

**生效配置** (Effective Config)：
合并与校验完成后，进程实际使用的配置值集合。
_Avoid_: 运行时配置、最终配置

**配置 schema** (Config Schema)：
声明全部配置项的唯一位置；每个配置项的路径、环境变量名、默认值、校验与文档都由它派生。
_Avoid_: 配置定义表、字段表

**镜像默认值** (Image Defaults)：
Docker 镜像相对内置默认值的差异集合，只包含镜像特有的配置项。
_Avoid_: 容器配置、运行时默认值

## 输出

**输出后端** (Output Backend)：
文件树对外提供的方式：`fuse`、`smb`，或 `none`（只保留 HTTP）。同一时刻只启用一个。
_Avoid_: 文件系统后端、模式、mode

## 身份

**凭据** (Credentials)：
一对 `username` / `password`，SMB 登录使用；HTTP 登录只校验 `password`。
_Avoid_: 账号、用户、共享凭据

## 实验

**上传统计实验** (Upload Statistics Experiment)：
通过 `[lab]` 配置的上报统计实验（baseline / inject / amplify），用于检验 tracker 的异常检测能力。
_Avoid_: 伪造上传、刷流
