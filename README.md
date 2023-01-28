# 機能

- Router Advertisement受信機能
  - Router Advertisementの受信(プレフィックス・ゲートウェイ)
    - 受信したプレフィックスは他の機能の設定時に利用可能
  - RouterOSへの各種設定反映機能
    - デフォルトゲートウェイの設定
    - インターフェースへのIPv6アドレス付与
    - IPv6 Poolへのプレフィックスの登録
- Neighbor Discoveryプロキシ(NDProxy)機能
  - 

# 設定方法

## RouterOS APIでTLSを使用する

RouterOS APIでTLSを使用するには、サーバー証明書を作成し設定する必要があります。
X.509証明書はRouterOS内で生成できますので、自己署名証明書を作成して使用します。

```
/certificate/add name=ca common-name=ca days-valid=3650 key-usage=key-cert-sign,crl-sign
/certificate/add name=apicert common-name=routerboard days-valid=3650
/certificate/sign ca
/certificate/sign apicert ca=ca
/ip/service/enable api-ssl
/ip/service/set api-ssl certificate=apicert
```

## 設定可能な環境変数

| キー             | デフォルト値      | 内容 |
| ---------------- | ----------------- | ---- |
| RA_MODE         | `ros`       | Router Advertisement受信機能の動作モードを指定します。<br> `off`: Router Advertisementに関する機能を無効化します<br> `ros`: RouterOS APIを用いてプレフィックス・IPをRouterBoardに付与し、プレフィックスをIPv6 Poolに格納します |
| RA_EXTERNAL_INTERFACES     | `eth0`            | 外部からのRAを受信するインターフェース(カンマ区切りで複数指定可能、最初に使用可能だったインターフェースを使用します)     |
| RA_ROS_EXTERNAL_INTERFACE | - | 外部ネットワークに面しているRouterOSインターフェースを指定します。このインターフェース向けにデフォルトルートが作成されます。受信したRAのゲートウェイを使用しない場合は指定しないでください。 |
| RA_ROS_EXTERNAL_IPS | - | 外部ネットワークに面しているインターフェースに割り当てるIPを`IPアドレス@インターフェース名`の形式で指定します。`ra-prefix`は受信したRAのプレフィックスに置き換えられます。`@external`は`RA_ROS_EXTERNAL_INTERFACE`で指定したインターフェースに置き換えられます。カンマ区切りで複数指定可能<br> ※とりあえずRouterBoardを外部から見えるようにしたい場合、`ra-prefix::1/128@@external`のように指定します<br> ※インターフェース名の後ろに`:`でオプションを付加することが可能です。利用可能なオプション: `:eui-64`、`:advertise` |
| RA_ROS_INTERNAL_IPS | - | 内部ネットワークに面しているインターフェースに割り当てるIPを`IPアドレス@インターフェース名`の形式で指定します(EXTERNAL_IPSと同様の形式)。カンマ区切りで複数指定可能 |
| RA_ROS_POOLS | `ra-prefix@fletsv6-pool/64` | 受信したプレフィックスを格納するIPv6 Poolを指定します。`プレフィックス@プール名/配下プレフィックス長`の形式で指定します。`none`で無指定 |
| RA_TIMEOUT | `5000` | Router Solicitation送信後のRouter Advertisement待機時間(ミリ秒) |
| NDP_MODE         | `proxy-ros`       | ND Proxyの動作モードを指定します。<br> `off`: 近隣探索に関する機能を無効化します<br> `static`: 内部での近隣探索を行わず、常に代理応答を送出します <br> `proxy`: 本プログラムが近隣探索を行います<br> `proxy-ros`: RouterOS APIを用いてRouterBoardから近隣探索を行います。※pingのみで到達可能なクライアントも外部に広告されます<br> `proxy-ros:strict`: proxy-rosと同じですが、RouterBoardから直接到達可能なクライアントのみが対象となります<br> ※`proxy`, `proxy-arp` は近隣探索成功時のみ代理応答を行います |
| NDP_PREFIXES       | `ra-prefix`       | ND Proxyの動作対象となるプレフィックスを指定します。`ra-prefix`は受信したRAのプレフィックスに置き換えられます。カンマ区切りで複数指定可能 |
| NDP_EXCLUDE_IPS    | `ra-externalips`     | ND Proxyの動作対象外となるIPアドレス/CIDRを指定します。`ra-externalips`と`ra-internalips`はそれぞれ、RA受信機能でRouterBoardに設定した外部IPアドレス、内部IPアドレスに置き換えられます。`ra-prefix`は受信したRAのプレフィックスに置き換えられます。カンマ区切りで複数指定可能、`none`で無指定 |
| NDP_EXTERNAL_INTERFACES  | `eth0`               | 外部からのND Solicitationが着信するインターフェース(カンマ区切りで複数指定可能)   |
| NDP_ADVERTISE_MACS | `@@external` | ND Advertisement送出時のソースMACアドレスを指定します。`@インターフェース名`と指定するとRouterOSの指定されたインターフェースのMACアドレスを取得して使用します。(RA機能使用時は`@external`も指定可能)カンマ区切りで複数指定可能、`ND_EXTERNAL_INTERFACES`の各項目と1:1で対応させます |
| NDP_INTERNAL_INTERFACES | ``               | 近隣探索を行う内部ネットワークのインターフェース(カンマ区切りで複数指定可能)          |
| NDP_TIMEOUT             | `1000` | 内部での近隣探索時の無応答タイムアウト(ミリ秒単位, `proxy-ros`の場合は10〜5000)
| ROS_HOST         | -                 | RouterOS API エンドポイント                   |
| ROS_PORT         | 8728(TLS時は8729) | RouterOS API 接続ポート                       |
| ROS_USER         | `admin`           | RouterOS API 接続ユーザー名                   |
| ROS_PASSWORD     | ``           | RouterOS API 接続ユーザー名                   |
| ROS_USETLS       | `0`               | RouterOS API接続時にTLSを利用するか(0 or 1)   |

※ インターフェースの指定時、`eth0@100`のように@をつけて指定すると特定のVLANタグを持つパケットのみを受信できます。なお、無指定のときはタグ付きとタグ無しの両方のパケットを受信します(タグ無しのパケットのみを受信することはできません)  
※ `ra-prefix`は単体でCIDRとして使うことも、サフィックスをつけてCIDR/IPとして使うこともできます。
例: プレフィックスが`2001:db8::/64`だったとき
- `ra-prefix` → `2001:db8::/64`
- `ra-prefix:1234:5678::/96` → `2001:db8:0:0:1234:5678::/96`
- `ra-prefix:1234:5678:9012:3456` → `2001:db8:0:0:1234:5678:9012:3456`