# Morde iOS 客户端源码

这里保留 Morde 原生 iOS 工程和它使用的 NanoMuse iOS 共享源码。工程位于 `android/src/ios/Morde.xcodeproj`，scheme 为 `Morde`；`Minis.xcodeproj` 与共享 iOS 源码一并保留，供当前工程复用。

本机无签名构建：

```sh
xcodebuild build \
  -project android/src/ios/Morde.xcodeproj \
  -scheme Morde \
  -configuration Debug \
  -destination 'generic/platform=iOS' \
  CODE_SIGNING_ALLOWED=NO
```

`android/deps` 只保留 iOS 工程用到的原生依赖构建源码和固定版本的 iSH 子模块；Android App、服务端、PC 客户端与手机 Web 源码分别位于仓库其他目录。保留本目录及 `android/` 下的许可证和第三方声明。
