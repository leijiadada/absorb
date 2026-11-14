---
name: mermaid
description: 用 mermaid 图表展示流程、时序、状态、实体关系。当用户要求画图/图示化，或回答内容适合用图表达（流程步骤、交互时序、状态转换、类与数据关系）时加载本技能，按模板输出 mermaid 代码块，由面板自动渲染成图。
---

# Mermaid 图表规范

把要表达的结构写成 ```mermaid 代码块直接输出在回复里（不要写文件、不要生成 HTML），面板会自动渲染成图。

## 语法红线（必须全部遵守）

1. 只用这 5 种图：flowchart、sequenceDiagram、stateDiagram-v2、classDiagram、erDiagram。不用 pie/gantt/mindmap 等。
2. 节点与边的文字一律用英文双引号包裹：`A["下单"] -->|"成功"| B["发货"]`；文字内不要出现英文双引号本身，引号语义用「」。
3. 节点 id 只用英文字母/数字（如 A、order、db1）；一行一条语句，缩进统一 4 空格。
4. 从下方模板改字生成：只替换节点 id、文字、连线关系，不要自创语法（style/classDef/subgraph 仅在模板出现时使用）。
5. 输出前自查：括号/方括号成对、箭头方向正确、participant/实体先声明后使用。
6. 图保持 3~10 个节点，过大拆成多图。

## 模板（复制后改字）

### flowchart（流程/架构）

```mermaid
flowchart TD
    A["用户下单"] --> B{"库存充足？"}
    B -->|"是"| C["扣减库存"]
    B -->|"否"| D["提示缺货"]
    C --> E["生成订单"]
```

### sequenceDiagram（交互时序）

```mermaid
sequenceDiagram
    participant U as 用户
    participant S as 服务端
    participant D as 数据库
    U->>S: 发起请求
    S->>D: 查询数据
    D-->>S: 返回结果
    S-->>U: 响应展示
```

### stateDiagram-v2（状态转换）

```mermaid
stateDiagram-v2
    [*] --> Idle
    Idle --> Loading: 开始请求
    Loading --> Success: 收到数据
    Loading --> Error: 请求失败
    Success --> Idle: 重置
    Error --> Idle: 重试
```

### classDiagram（类/模块关系）

```mermaid
classDiagram
    class User {
        +String id
        +String name
        +login()
    }
    class Order {
        +String id
        +Decimal total
        +pay()
    }
    User "1" --> "*" Order: creates
```

### erDiagram（数据实体关系）

```mermaid
erDiagram
    USER ||--o{ ORDER : places
    USER {
        string id PK
        string name
    }
    ORDER {
        string id PK
        string user_id FK
        decimal total
    }
```

## 选择指引

- 步骤/分支/架构分层 → flowchart；多方消息交互 → sequenceDiagram；生命周期/状态机 → stateDiagram-v2；代码结构 → classDiagram；表/存储关系 → erDiagram。
- 中文标签必须引号包裹；同一图内连线关系表达主要逻辑，次要细节用文字补充说明。
