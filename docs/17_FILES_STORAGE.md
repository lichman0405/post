# Files、Blob 与对象存储

## 1. Files 是只读投影

Files 页面展示 underlying Git tree 和 blob references，不是写入口。Web 不提供 Edit/Upload/Replace/Delete。

## 2. 存储分层

- 小文本/manifest/scripts：Git backend。
- 大数据/仪器输出/trajectory/images：S3-compatible object storage。
- Git 中保存 immutable pointer/manifest，不强制所有大数据进 Git LFS；V1 平台自行维护 blob ref 与 hash。

## 3. Blob identity

Blob 以 content hash + blob id 标识。记录 size、media type、storage key、encryption metadata、created actor、attached objects、access policy、integrity status。

## 4. Upload path

**V1 不含用户可见的上传入口**（`L3-②` 裁定，`docs/02` §4）：契约不声明 `blobs:request-upload` /
`blobs/{blobId}:finalize`，Web 无上传 UI，V1 的 blob 只可能由平台内部流程产生。以下约束
对 V1.x 的上传路径与任何内部写入同样成立。

上传必须从科研上下文发生：Experiment raw data、Calculation output、Dataset files、Protocol attachment、Agent semantic operation。所有 finalize 必须指定 object/branch/purpose。

## 5. Download

每次下载检查 Project/Object/Blob access policy；生成短期 signed URL。Public metadata 不代表 blob open download。

## 6. Retention

一旦 blob 被 accepted state/release/asset version 引用，不物理删除。仅可变更 current accessibility；合规删除请求属于未来 legal exception，不得伪装为普通 delete。
