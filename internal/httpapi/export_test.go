package httpapi

// MaxTrendPointsForTest 把趋势的点数上限暴露给外部测试包。
//
// 导出一个测试专用别名而不是把常量本身导出：它是内部实现细节，而外部包要
// 断言的是"handler 要的点数与存储层肯给的点数一致" —— 那条断言必须读到
// 真实的那个值，抄一份进测试就再也守不住任何东西。
const MaxTrendPointsForTest = maxTrendPoints
