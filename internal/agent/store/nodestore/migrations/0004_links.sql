-- 跨节点连接共享网络：本节点的网络和哪些节点的同名网络相连（双方互相记着才算连通）。
CREATE TABLE shared_network_links (
    network   TEXT NOT NULL,
    peer_key  BLOB NOT NULL,
    peer_name TEXT NOT NULL,
    peer_node TEXT NOT NULL,
    PRIMARY KEY (network, peer_key)
);
