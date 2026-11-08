// 后端调用的唯一入口。
//
// 直接导出 Wails 生成的 CoreService：类型由绑定生成器保证，
// 不再手写一份等价签名再用 `as unknown as` 强行断言——那种写法
// 一旦后端签名变化，编译器不会报警，只会在运行时炸。

import { CoreService } from "../bindings/proxdeck";

export const svc = CoreService;
