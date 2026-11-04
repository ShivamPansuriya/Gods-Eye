export interface Order {
  id: string;
  tenantId: string;
  customerId: string;
  totalCents: number;
  status: "open" | "paid" | "refunded" | "deleted";
  createdAt: Date;
}

export interface Db {
  query<T>(sql: string, params: unknown[]): Promise<T[]>;
  exec(sql: string, params: unknown[]): Promise<{ rowCount: number }>;
}

export interface Payments {
  refund(orderId: string, amountCents: number): Promise<void>;
}
