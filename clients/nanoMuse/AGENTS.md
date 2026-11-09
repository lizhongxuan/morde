# Morde iOS 客户端约定

- 本目录只放 Morde iOS 工程、它复用的 iOS 源码、iOS 原生依赖构建材料和许可证文件。
- Morde 工程为 `android/src/ios/Morde.xcodeproj`，scheme 为 `Morde`。不要把 Android App、PC 客户端、Cloud 服务或 Web 源码放回这里。
- 修改 NanoMuse 或 OpenMinis 来源文件时保留上游版权和许可声明；不要覆盖 `LICENSE`、`NOTICE` 或第三方许可文件。
- 新增用户可见文字时同步更新 `Localizable.xcstrings` 中现有语言。
