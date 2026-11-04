import { Db, Order, Payments } from "./db";

export class NotFound extends Error {}

/** Every query is scoped to the caller's tenant. */
export class OrderService {
  constructor(private db: Db, private payments: Payments) {}

  async get(tenantId: string, id: string): Promise<Order> {
    const rows = await this.db.query<Order>(
      "SELECT * FROM orders WHERE id = $1 AND tenant_id = $2 AND status <> 'deleted'",
      [id, tenantId],
    );
    if (rows.length === 0) throw new NotFound(id);
    return rows[0];
  }

  async markPaid(tenantId: string, id: string): Promise<void> {
    const res = await this.db.exec(
      "UPDATE orders SET status = 'paid' WHERE id = $1 AND tenant_id = $2",
      [id, tenantId],
    );
    if (res.rowCount === 0) throw new NotFound(id);
  }
}
