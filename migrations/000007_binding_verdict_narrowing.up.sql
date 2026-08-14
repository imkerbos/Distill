-- 把绑定上越界的历史结论收窄到路径级枚举内（final review I2）。
--
-- 000006 把校验结论按层拆成两个各自封闭的枚举：仓库级六值，路径级三值
-- （NOT_VERIFIED / OK / PATH_MISSING）。它仔细处理了**新建的** git_repo 行，
-- 却没有动留在原地的 cluster_git_binding.verify_result —— 那一列在 000004
-- 引入时收的是压在一起的旧结论，因此可能存着 AUTH_FAILED、
-- CREDENTIAL_UNRESOLVED、REPO_UNREACHABLE、BRANCH_MISSING，
-- 全是升级后这一层不存在的取值。
--
-- 后果不是显示难看，是功能被永久堵死：handleVerifyGitBinding 会对**从库里
-- 读出来的**绑定调 registry.ValidateGitBinding，未登记取值直接被拒，于是那个
-- 集群此后每一次「重新校验」都返回同一句「不在已登记的取值范围内」，
-- 操作者永远拿不到新结论，界面上也没有任何东西会告诉他该重新绑定一次。
--
-- 归到 NOT_VERIFIED，理由与 000006 写在 git_repo 那段逐字同源：这些行携带的
-- 是一个**这一层从未单独做过的判断**。一个认不出来的结论绝不能被解释成通过
-- 的结论（CLAUDE.md §3：无法确定就返回未校验，不朝可信的方向凑）；也不能
-- 解释成 PATH_MISSING —— 那会把操作者送去改一个其实没问题的 policyPath。
-- NOT_VERIFIED 是三个取值里唯一一个不替平台声明任何事实的。
--
-- verified_at 一并清空：结论没了，那个时刻就不再是任何东西的时刻，
-- 留着它会在界面上显示成一次从未发生过的校验（同 000006 的处置）。
UPDATE cluster_git_binding
   SET verify_result = 'NOT_VERIFIED',
       verified_at   = NULL
 WHERE verify_result NOT IN ('NOT_VERIFIED', 'OK', 'PATH_MISSING');
