package controller

type sessionNotifications struct {
	profile string
	notify  func(string)
	last    verifiedNode
	lost    bool
}

func (n *sessionNotifications) emit(event, node string) {
	if n.notify != nil {
		n.notify(event + " · " + n.profile + " · " + node)
	}
}

func (n *sessionNotifications) observe(node verifiedNode) {
	if node.id == "" {
		if n.last.id != "" && !n.lost {
			n.lost = true
			n.emit("Connection lost", n.last.label)
		}
		return
	}
	label := node.label
	if n.last.id != "" && n.last.id != node.id {
		previous := n.last.label
		if previous == label {
			previous += " (" + n.last.id + ")"
			label += " (" + node.id + ")"
		}
		label = previous + " → " + label
	}
	switch {
	case n.last.id == "":
		n.emit("Connected", label)
	case n.lost:
		n.emit("Connection recovered", label)
	case n.last.id != node.id:
		n.emit("Node switched", label)
	}
	n.last, n.lost = node, false
}

func (n *sessionNotifications) disconnect() {
	if n.last.id != "" {
		n.emit("Disconnected", n.last.label)
	}
}
