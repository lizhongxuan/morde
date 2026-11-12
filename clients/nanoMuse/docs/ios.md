# Morde iOS

本目录只保留 iOS 客户端、共享 iOS 源码、原生依赖构建材料和对应许可。

## 工程

- 主工程：`android/src/ios/Morde.xcodeproj`，scheme `Morde`。
- NanoMuse 原生共享源码和资源位于 `android/src/ios/NanoMuse/`。
- `Minis.xcodeproj` 是保留的历史融合工程；它使用 `android/deps/` 中的 iSH、FFmpeg 和 rclone 依赖。

## 构建 Morde

需要 macOS 与 Xcode。在仓库根目录执行：

```sh
xcodebuild build \
  -project clients/nanoMuse/android/src/ios/Morde.xcodeproj \
  -scheme Morde \
  -configuration Debug \
  -destination 'generic/platform=iOS' \
  CODE_SIGNING_ALLOWED=NO
```

该命令只用于未签名的设备目标编译，不能用于 iOS Simulator Keychain 登录验收。Simulator 的 UI/登录测试需保持代码签名开启，使 Xcode 为应用签入 `Morde.entitlements` 中的默认 Keychain access group；实体设备验收还需可用的 Apple Development 身份及匹配的 provisioning profile。

需要运行历史 `Minis` scheme 时，先初始化固定版本的 iSH 子模块：

```sh
git submodule update --init clients/nanoMuse/android/deps/ish
```

然后按需使用 `clients/nanoMuse/android/deps/` 下的 iOS 构建脚本。构建生成物和本机凭据不会纳入版本控制。

## 本地静态检查

`tests/test_ios_strings.py` 检查 iOS 字符串目录是否有重复键，以及 NanoMuse 自有字符串是否覆盖现有语言。可在 NanoMuse 目录运行：

```sh
python3 -m pytest tests/test_ios_strings.py
```
