import Foundation
import Network
import Observation
import UserNotifications

@Observable
final class RelayConnection {
    var agents: [Agent] = []
    var isConnected = false
    var hostAddress = "ws://127.0.0.1:8375"

    private var task: URLSessionWebSocketTask?
    private var browser: NWBrowser?
    private let session = URLSession(configuration: .default)
    private var reconnectAttempt = 0
    private var reconnecting = false

    init() {
        // Connect to localhost immediately — don't wait for Bonjour
        connect(to: hostAddress)
        startBrowsing()
    }

    func startBrowsing() {
        let params = NWParameters()
        params.includePeerToPeer = true
        browser = NWBrowser(for: .bonjour(type: "_herdi._tcp", domain: nil), using: params)
        browser?.browseResultsChangedHandler = { [weak self] results, _ in
            guard let self, !self.isConnected else { return }
            guard let result = results.first else { return }
            if case let .service(name, type, domain, _) = result.endpoint {
                self.resolve(name: name, type: type, domain: domain)
            }
        }
        browser?.start(queue: .main)
    }

    private func resolve(name: String, type: String, domain: String) {
        let connection = NWConnection(to: .service(name: name, type: type, domain: domain, interface: nil), using: .tcp)
        connection.stateUpdateHandler = { [weak self] state in
            if case .ready = state,
               let endpoint = connection.currentPath?.remoteEndpoint,
               case let .hostPort(host, port) = endpoint {
                let addr = "\(host)".replacingOccurrences(of: "%.*", with: "", options: .regularExpression)
                DispatchQueue.main.async {
                    guard let self, !self.isConnected else { return }
                    self.connect(to: "ws://\(addr):\(port)")
                }
                connection.cancel()
            }
        }
        connection.start(queue: .global())
    }

    func connect(to urlString: String) {
        guard let url = URL(string: urlString) else { return }
        hostAddress = urlString
        reconnecting = false
        task?.cancel(with: .normalClosure, reason: nil)
        task = session.webSocketTask(with: url)
        task?.resume()
        isConnected = true
        reconnectAttempt = 0
        listen()
    }

    func disconnect() {
        task?.cancel(with: .normalClosure, reason: nil)
        isConnected = false
    }

    func send(response: ResponseMessage) {
        guard let data = try? JSONEncoder().encode(response) else { return }
        task?.send(.string(String(data: data, encoding: .utf8)!)) { _ in }
    }

    private func listen() {
        task?.receive { [weak self] result in
            guard let self else { return }
            switch result {
            case .success(let message):
                switch message {
                case .string(let text): self.handle(text)
                case .data(let data): self.handle(String(data: data, encoding: .utf8) ?? "")
                @unknown default: break
                }
                self.listen()
            case .failure:
                DispatchQueue.main.async {
                    self.isConnected = false
                    self.scheduleReconnect()
                }
            }
        }
    }

    private func scheduleReconnect() {
        guard !reconnecting else { return }
        reconnecting = true
        reconnectAttempt += 1
        let delay = min(Double(1 << min(reconnectAttempt, 5)), 30.0)
        DispatchQueue.main.asyncAfter(deadline: .now() + delay) { [weak self] in
            guard let self, !self.isConnected else { return }
            self.reconnecting = false
            self.connect(to: self.hostAddress)
        }
    }

    private func handle(_ text: String) {
        guard let data = text.data(using: .utf8),
              let msg = try? JSONDecoder().decode(AgentMessage.self, from: data) else { return }
        DispatchQueue.main.async { [self] in
            switch msg.type {
            case "agents":
                guard let list = msg.agents else { return }
                // Update in-place to avoid view thrashing
                var seen = Set<String>()
                for a in list {
                    seen.insert(a.pane_id)
                    if let existing = agents.first(where: { $0.id == a.pane_id }) {
                        let newStatus = AgentStatus(rawValue: a.status) ?? .unknown
                        if existing.status != newStatus { existing.status = newStatus }
                        if existing.project != a.project { existing.project = a.project }
                        if existing.host != (a.host ?? "local") { existing.host = a.host ?? "local" }
                    } else {
                        agents.append(Agent(
                            id: a.pane_id, name: a.agent,
                            status: AgentStatus(rawValue: a.status) ?? .unknown,
                            project: a.project, cwd: a.cwd, host: a.host ?? "local"
                        ))
                    }
                }
                agents.removeAll { !seen.contains($0.id) }

            case "blocked":
                if let pid = msg.pane_id, let agent = agents.first(where: { $0.id == pid }) {
                    agent.prompt = msg.prompt
                    agent.options = msg.options
                    agent.status = .blocked
                    sendNotification(agent: agent.name, project: agent.project)
                }
            default: break
            }
        }
    }

    private func sendNotification(agent: String, project: String) {
        let center = UNUserNotificationCenter.current()
        let content = UNMutableNotificationContent()
        content.title = "Agent Blocked"
        content.body = "\(agent) needs input in \(project)"
        content.sound = .default
        center.add(UNNotificationRequest(identifier: UUID().uuidString, content: content, trigger: nil))
    }
}
