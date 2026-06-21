// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "HerdiMac",
    platforms: [.macOS(.v14)],
    targets: [
        .executableTarget(
            name: "HerdiMac",
            path: "Sources"
        )
    ]
)
