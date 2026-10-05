"""chronotope-harness：无状态单段执行器。

纪律（mvp-落地方案 §9.2）：无 Session、无本地存储；模型调用永远走 LiteLLM；
工具分流（API 内联 / CODE 交棒）；三种返回 done / tool_call / error。
"""

__version__ = "0.1.0"
