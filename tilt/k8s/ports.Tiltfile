# ports.tilt - Centralized port management

# Port registry - SINGLE SOURCE OF TRUTH
PORT_REGISTRY = {
    # Infrastructure
    "redis": 6379,

    # Validator
    "validator_grpc": 9090,
    "validator_rpc": 26657,
    "validator_rest": 1317,

    # Backend
    "backend_http": 8545,
    "backend_grpc": 50051,
    "backend_metrics": 9095,

    # Observability
    "grafana": 3000,
    "prometheus": 9091,
}

def get_port(service_name):
    """Get port for a service from registry"""
    if service_name not in PORT_REGISTRY:
        fail("Unknown service: {}".format(service_name))
    return PORT_REGISTRY[service_name]

def get_relayer_ports(instance_num, base_port, metrics_base, health_base, pprof_port):
    """Generate ports for a relayer instance (eliminates arithmetic)"""
    return {
        "relay": base_port + instance_num - 1,
        "metrics": metrics_base + instance_num - 1,
        "health": health_base + instance_num - 1,
        "pprof": pprof_port + instance_num - 1,
    }

def get_miner_ports(instance_num, metrics_base, pprof_port):
    """Generate ports for a miner instance"""
    return {
        "metrics": metrics_base + instance_num - 1,
        "pprof": pprof_port + instance_num - 1,
    }

def validate_port_conflicts(config):
    """Validate no port conflicts in configuration"""
    used_ports = {}

    # Check infrastructure ports
    for service, port in PORT_REGISTRY.items():
        used_ports[port] = service

    # Check relayers
    for i in range(config["relayer"]["count"]):
        ports = get_relayer_ports(
            i + 1,
            config["relayer"]["base_port"],
            config["relayer"]["metrics_base_port"],
            config["relayer"]["health_base_port"],
            config["relayer"]["pprof_port"],
        )
        for name, port in ports.items():
            if port in used_ports:
                fail("Port conflict: relayer{}_{} and {} both use port {}".format(
                    i + 1, name, used_ports[port], port
                ))
            used_ports[port] = "relayer{}_{}".format(i + 1, name)

    # Check miners
    for i in range(config["miner"]["count"]):
        ports = get_miner_ports(
            i + 1,
            config["miner"]["metrics_base_port"],
            config["miner"]["pprof_port"],
        )
        for name, port in ports.items():
            if port in used_ports:
                fail("Port conflict: miner{}_{} and {} both use port {}".format(
                    i + 1, name, used_ports[port], port
                ))
            used_ports[port] = "miner{}_{}".format(i + 1, name)

    return True
