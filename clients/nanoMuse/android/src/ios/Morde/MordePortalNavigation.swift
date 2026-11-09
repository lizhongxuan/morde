import Foundation

func isAllowedMordePortalNavigation(_ candidate: URL?, relativeTo origin: URL) -> Bool {
    guard let candidate,
          let candidateComponents = URLComponents(url: candidate, resolvingAgainstBaseURL: false),
          let originComponents = URLComponents(url: origin, resolvingAgainstBaseURL: false),
          let candidateServiceOrigin = MordeLoginStateStore.serviceOrigin(candidate.absoluteString),
          let originServiceOrigin = MordeLoginStateStore.serviceOrigin(origin.absoluteString),
          candidateServiceOrigin == originServiceOrigin,
          let candidateHost = candidateComponents.host, !candidateHost.isEmpty,
          let originHost = originComponents.host, !originHost.isEmpty,
          candidateHost.caseInsensitiveCompare(originHost) == .orderedSame,
          candidateComponents.user == nil, candidateComponents.password == nil,
          originComponents.user == nil, originComponents.password == nil,
          originComponents.percentEncodedPath.isEmpty || originComponents.percentEncodedPath == "/",
          originComponents.query == nil, originComponents.fragment == nil else { return false }

    return ["/", "/assistant", "/hosts", "/connections", "/apikeys"]
        .contains(candidateComponents.percentEncodedPath)
}
