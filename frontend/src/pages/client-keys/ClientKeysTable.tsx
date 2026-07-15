import { useState, type FormEvent } from "react";
import { ChevronLeft, ChevronRight, Search } from "lucide-react";
import type { ClientKey } from "@/api/client";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { formatClientKeyDate } from "@/pages/client-keys/clientKeyDraft";

export function ClientKeysTable({
  items,
  total,
  page,
  pageSize,
  q,
  origin,
  loading,
  onSearch,
  onOrigin,
  onPage,
  onOpenDetail,
}: {
  items: ClientKey[];
  total: number;
  page: number;
  pageSize: number;
  q: string;
  origin: string;
  loading: boolean;
  onSearch: (value: string) => void;
  onOrigin: (value: string) => void;
  onPage: (value: number) => void;
  onOpenDetail: (key: ClientKey) => void;
}) {
  const [qDraft, setQDraft] = useState(q);
  const totalPages = Math.max(1, Math.ceil(total / pageSize));

  function submit(event: FormEvent) {
    event.preventDefault();
    onSearch(qDraft.trim());
  }

  return (
    <>
      <Card>
        <CardContent className="p-3">
          <form className="flex flex-col gap-2 sm:flex-row" onSubmit={submit}>
            <div className="relative min-w-0 flex-1">
              <Label className="sr-only" htmlFor="client-key-search">搜索客户端密钥</Label>
              <Search className="pointer-events-none absolute left-3 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input
                id="client-key-search"
                className="pl-8"
                value={qDraft}
                onChange={(event) => setQDraft(event.target.value)}
                placeholder="搜索名称或密钥前缀"
              />
            </div>
            <Label className="sr-only" htmlFor="client-key-origin">来源</Label>
            <select
              id="client-key-origin"
              className="h-8 rounded-md border border-input bg-secondary/55 px-3 text-xs outline-none focus-visible:ring-1 focus-visible:ring-ring"
              value={origin}
              onChange={(event) => onOrigin(event.target.value)}
            >
              <option value="">全部来源</option>
              <option value="managed">面板创建</option>
              <option value="config_api_key">历史配置</option>
            </select>
            <Button type="submit" size="sm">查询</Button>
          </form>
        </CardContent>
      </Card>

      <Card className="overflow-hidden">
        <div className="overflow-x-auto">
          <table className="w-full min-w-[900px] text-left text-xs">
            <thead className="border-b border-border/80 bg-background text-[11px] text-muted-foreground">
              <tr>
                <th className="px-3 py-2.5 font-medium">名称</th>
                <th className="px-3 py-2.5 font-medium">状态</th>
                <th className="px-3 py-2.5 font-medium">模型权限</th>
                <th className="px-3 py-2.5 text-right font-medium">RPM</th>
                <th className="px-3 py-2.5 text-right font-medium">并发</th>
                <th className="px-3 py-2.5 font-medium">最后使用</th>
                <th className="px-3 py-2.5 text-right font-medium">操作</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-border/70">
              {items.map((item) => (
                <tr key={item.id} className="transition-colors hover:bg-background">
                  <td className="px-3 py-3">
                    <div className="font-medium text-foreground">{item.name}</div>
                    <div className="mt-0.5 font-mono text-[11px] text-muted-foreground">{item.key_prefix}</div>
                  </td>
                  <td className="px-3 py-3">
                    <Badge tone={item.revoked_at ? "danger" : "success"}>
                      {item.revoked_at ? "已撤销" : "有效"}
                    </Badge>
                  </td>
                  <td className="px-3 py-3">
                    {item.model_policy === "all" ? "全部模型" : `${item.model_scopes.length} 个模型`}
                  </td>
                  <td className="px-3 py-3 text-right tabular-nums">
                    {item.rpm_limit === 0 ? "不限" : item.rpm_limit}
                  </td>
                  <td className="px-3 py-3 text-right tabular-nums">
                    {item.max_concurrent === 0 ? "不限" : item.max_concurrent}
                  </td>
                  <td className="px-3 py-3 text-muted-foreground">
                    {formatClientKeyDate(item.last_used_at)}
                  </td>
                  <td className="px-3 py-3 text-right">
                    <Button
                      variant="ghost"
                      size="sm"
                      aria-label={`查看 ${item.name}`}
                      onClick={() => onOpenDetail(item)}
                    >
                      查看
                    </Button>
                  </td>
                </tr>
              ))}
              {!loading && items.length === 0 ? (
                <tr>
                  <td colSpan={7} className="px-3 py-12 text-center text-muted-foreground">
                    暂无客户端密钥
                  </td>
                </tr>
              ) : null}
              {loading && items.length === 0 ? (
                <tr>
                  <td colSpan={7} className="px-3 py-12 text-center text-muted-foreground">
                    正在加载客户端密钥…
                  </td>
                </tr>
              ) : null}
            </tbody>
          </table>
        </div>
        <div className="flex items-center justify-between border-t border-border/70 bg-background px-3 py-2.5">
          <span className="text-[11px] text-muted-foreground">
            共 {total} 条 · 第 {page}/{totalPages} 页
          </span>
          <div className="flex gap-1">
            <Button
              variant="outline"
              size="sm"
              disabled={page <= 1 || loading}
              onClick={() => onPage(Math.max(1, page - 1))}
            >
              <ChevronLeft className="size-3.5" />上一页
            </Button>
            <Button
              variant="outline"
              size="sm"
              disabled={page >= totalPages || loading}
              onClick={() => onPage(page + 1)}
            >
              下一页<ChevronRight className="size-3.5" />
            </Button>
          </div>
        </div>
      </Card>
    </>
  );
}
