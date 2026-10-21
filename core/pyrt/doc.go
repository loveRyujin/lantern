// Package pyrt 检测本机 Python，并在应用数据目录准备带哈希锁定的专用 venv。
//
// 检测顺序是：Config.Python（设置里指定的解释器；指定后不再改用别的）→
// PATH 上的 python3 → DefaultSearchPaths（Homebrew、/usr/local、/usr/bin）。
// 解释器必须 ≥ 3.10。Ensure 还会按嵌入的 requirements.lock 收窄到锁文件声明的范围。
//
// venv 位于 <Root>/pyenv/venv。创建 venv 和 pip install 都在沙箱外执行，需要联网。
// 安装使用 pip 的 --require-hashes 和 --only-binary=:all:，版本和 sha256 写在锁文件里。
// 调用 pip 时去掉 PIP_TARGET、PIP_PREFIX、PIP_USER，避免包装到 venv 外面。
// 写成功标记之前会在 venv 里 import pandas 和 matplotlib。
// 标记对不上时，在 Root 内的临时目录生成新 venv，再原子 rename 替换旧目录，不对已有目录使用 --clear。
// Root、Root/pyenv 或其路径上任一层是符号链接，或解析后不在 Root 内时，返回 path_escape。
// 同一个 Root 上不要并发调用 Ensure。
//
// 解释器缺失、版本不符、venv 创建失败、锁定依赖装不上或 import 失败时，返回的错误实现 errcode.Coded，
// Code() 为 runtime_unavailable。调用方不得因此改为不经沙箱直接执行。
// 路径逃逸的码是 path_escape。上下文取消和 Root 为空不带错误码。
package pyrt
