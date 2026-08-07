package main

// Demo specs. They are written out in full rather than generated so that they
// read like specs a real service would publish: shared components referenced
// from several operations, a legacy Swagger 2.0 document, mixed OpenAPI 3.0 and
// 3.1 — which is exactly the variety the portal has to cope with.

const ordersSpec = `{
  "openapi": "3.0.3",
  "info": {
    "title": "Orders API",
    "description": "Приём и обработка заказов.",
    "version": "2.4.1"
  },
  "servers": [{"url": "http://localhost:7101"}],
  "tags": [{"name": "orders", "description": "Заказы"}],
  "paths": {
    "/orders": {
      "get": {
        "tags": ["orders"],
        "operationId": "listOrders",
        "summary": "Список заказов",
        "parameters": [
          {"name": "status", "in": "query", "required": false,
           "schema": {"type": "string", "enum": ["new", "paid", "shipped", "cancelled"]}},
          {"name": "limit", "in": "query", "required": false,
           "schema": {"type": "integer", "default": 50, "maximum": 500}}
        ],
        "responses": {
          "200": {"description": "OK", "content": {"application/json": {"schema": {
            "type": "array", "items": {"$ref": "#/components/schemas/Order"}}}}},
          "400": {"description": "Bad request", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Error"}}}}
        }
      },
      "post": {
        "tags": ["orders"],
        "operationId": "createOrder",
        "summary": "Создать заказ",
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/CreateOrder"}}}},
        "responses": {
          "201": {"description": "Created", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Order"}}}},
          "400": {"description": "Bad request", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Error"}}}}
        }
      }
    },
    "/orders/{id}": {
      "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string", "format": "uuid"}}],
      "get": {
        "tags": ["orders"],
        "operationId": "getOrder",
        "summary": "Заказ по идентификатору",
        "responses": {
          "200": {"description": "OK", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Order"}}}},
          "404": {"description": "Not found", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Error"}}}}
        }
      },
      "delete": {
        "tags": ["orders"],
        "operationId": "cancelOrder",
        "summary": "Отменить заказ",
        "responses": {
          "204": {"description": "Cancelled"},
          "404": {"description": "Not found", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Error"}}}}
        }
      }
    }
  },
  "components": {
    "schemas": {
      "Order": {
        "type": "object",
        "required": ["id", "status", "items", "total"],
        "properties": {
          "id": {"type": "string", "format": "uuid"},
          "status": {"type": "string", "enum": ["new", "paid", "shipped", "cancelled"]},
          "createdAt": {"type": "string", "format": "date-time"},
          "items": {"type": "array", "items": {"$ref": "#/components/schemas/OrderItem"}},
          "total": {"type": "number", "format": "double"}
        }
      },
      "OrderItem": {
        "type": "object",
        "required": ["sku", "quantity", "price"],
        "properties": {
          "sku": {"type": "string"},
          "title": {"type": "string"},
          "quantity": {"type": "integer", "minimum": 1},
          "price": {"type": "number", "format": "double"}
        }
      },
      "CreateOrder": {
        "type": "object",
        "required": ["customerId", "items"],
        "properties": {
          "customerId": {"type": "string", "format": "uuid"},
          "items": {"type": "array", "minItems": 1, "items": {"$ref": "#/components/schemas/OrderItem"}},
          "comment": {"type": "string", "maxLength": 500}
        }
      },
      "Error": {
        "type": "object",
        "required": ["code", "message"],
        "properties": {"code": {"type": "string"}, "message": {"type": "string"}}
      }
    }
  }
}`

const billingSpec = `{
  "openapi": "3.0.1",
  "info": {
    "title": "Billing API",
    "description": "Счета и платежи по подписке.",
    "version": "1.9.0"
  },
  "servers": [{"url": "http://localhost:7102"}],
  "paths": {
    "/invoices": {
      "get": {
        "operationId": "listInvoices",
        "summary": "Список счетов",
        "parameters": [
          {"name": "customerId", "in": "query", "required": true, "schema": {"type": "string"}},
          {"name": "paid", "in": "query", "required": false, "schema": {"type": "boolean"}}
        ],
        "responses": {
          "200": {"description": "OK", "content": {"application/json": {"schema": {
            "type": "array", "items": {"$ref": "#/components/schemas/Invoice"}}}}},
          "401": {"description": "Unauthorized"}
        }
      }
    },
    "/invoices/{id}": {
      "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
      "get": {
        "operationId": "getInvoice",
        "summary": "Счёт по идентификатору",
        "responses": {
          "200": {"description": "OK", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Invoice"}}}},
          "404": {"description": "Not found", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Error"}}}}
        }
      }
    },
    "/invoices/{id}/pay": {
      "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
      "post": {
        "operationId": "payInvoice",
        "summary": "Оплатить счёт",
        "requestBody": {"required": true, "content": {"application/json": {"schema": {
          "type": "object", "required": ["method"],
          "properties": {"method": {"type": "string", "enum": ["card", "wire", "balance"]},
                         "amount": {"$ref": "#/components/schemas/Money"}}}}}},
        "responses": {
          "200": {"description": "Paid", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Invoice"}}}},
          "402": {"description": "Payment required", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Error"}}}},
          "404": {"description": "Not found"}
        }
      }
    }
  },
  "components": {
    "schemas": {
      "Invoice": {
        "type": "object",
        "required": ["id", "customerId", "amount", "paid"],
        "properties": {
          "id": {"type": "string"},
          "customerId": {"type": "string"},
          "amount": {"$ref": "#/components/schemas/Money"},
          "paid": {"type": "boolean"},
          "dueDate": {"type": "string", "format": "date"}
        }
      },
      "Money": {
        "type": "object",
        "required": ["value", "currency"],
        "properties": {
          "value": {"type": "number", "format": "double"},
          "currency": {"type": "string", "enum": ["RUB", "USD", "EUR"]}
        }
      },
      "Error": {
        "type": "object",
        "properties": {"code": {"type": "string"}, "message": {"type": "string"}}
      }
    }
  }
}`

const inventorySpec = `{
  "openapi": "3.1.0",
  "info": {
    "title": "Inventory API",
    "description": "Остатки товаров по складам.",
    "version": "0.14.2"
  },
  "servers": [{"url": "http://localhost:7103"}],
  "paths": {
    "/items": {
      "get": {
        "operationId": "listItems",
        "summary": "Список позиций",
        "parameters": [
          {"name": "warehouse", "in": "query", "required": false, "schema": {"type": "string"}},
          {"name": "inStock", "in": "query", "required": false, "schema": {"type": "boolean"}}
        ],
        "responses": {
          "200": {"description": "OK", "content": {"application/json": {"schema": {
            "type": "array", "items": {"$ref": "#/components/schemas/Item"}}}}}
        }
      }
    },
    "/items/{sku}": {
      "parameters": [{"name": "sku", "in": "path", "required": true, "schema": {"type": "string"}}],
      "get": {
        "operationId": "getItem",
        "summary": "Позиция по SKU",
        "responses": {
          "200": {"description": "OK", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}},
          "404": {"description": "Not found", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Error"}}}}
        }
      },
      "put": {
        "operationId": "upsertItem",
        "summary": "Создать или обновить позицию",
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}},
        "responses": {
          "200": {"description": "Updated", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}},
          "201": {"description": "Created", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}}
        }
      }
    },
    "/warehouses": {
      "get": {
        "operationId": "listWarehouses",
        "summary": "Список складов",
        "responses": {
          "200": {"description": "OK", "content": {"application/json": {"schema": {
            "type": "array", "items": {"$ref": "#/components/schemas/Warehouse"}}}}}
        }
      }
    }
  },
  "components": {
    "schemas": {
      "Item": {
        "type": "object",
        "required": ["sku", "quantity"],
        "properties": {
          "sku": {"type": "string"},
          "title": {"type": "string"},
          "quantity": {"type": "integer", "minimum": 0},
          "warehouse": {"$ref": "#/components/schemas/Warehouse"}
        }
      },
      "Warehouse": {
        "type": "object",
        "required": ["code"],
        "properties": {
          "code": {"type": "string"},
          "city": {"type": "string"}
        }
      },
      "Error": {
        "type": "object",
        "properties": {"code": {"type": "string"}, "message": {"type": "string"}}
      }
    }
  }
}`

const notificationsSpec = `{
  "openapi": "3.0.3",
  "info": {
    "title": "Notifications API",
    "description": "Подписки и доставка уведомлений.",
    "version": "3.2.0"
  },
  "servers": [{"url": "http://localhost:7104"}],
  "paths": {
    "/subscriptions": {
      "get": {
        "operationId": "listSubscriptions",
        "summary": "Подписки пользователя",
        "parameters": [{"name": "userId", "in": "query", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "OK", "content": {"application/json": {"schema": {
            "type": "array", "items": {"$ref": "#/components/schemas/Subscription"}}}}}
        }
      },
      "post": {
        "operationId": "createSubscription",
        "summary": "Подписаться на канал",
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Subscription"}}}},
        "responses": {
          "201": {"description": "Created", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Subscription"}}}},
          "409": {"description": "Already subscribed"}
        }
      }
    },
    "/subscriptions/{id}": {
      "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
      "delete": {
        "operationId": "deleteSubscription",
        "summary": "Отписаться",
        "responses": {"204": {"description": "Deleted"}, "404": {"description": "Not found"}}
      }
    },
    "/notifications/send": {
      "post": {
        "operationId": "sendNotification",
        "summary": "Отправить уведомление",
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/SendRequest"}}}},
        "responses": {
          "202": {"description": "Accepted"},
          "400": {"description": "Bad request", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Error"}}}}
        }
      }
    }
  },
  "components": {
    "schemas": {
      "Subscription": {
        "type": "object",
        "required": ["userId", "channel"],
        "properties": {
          "id": {"type": "string"},
          "userId": {"type": "string"},
          "channel": {"type": "string", "enum": ["email", "sms", "push", "webhook"]},
          "target": {"type": "string"}
        }
      },
      "SendRequest": {
        "type": "object",
        "required": ["userId", "template"],
        "properties": {
          "userId": {"type": "string"},
          "template": {"type": "string"},
          "payload": {"type": "object", "additionalProperties": true}
        }
      },
      "Error": {
        "type": "object",
        "properties": {"code": {"type": "string"}, "message": {"type": "string"}}
      }
    }
  }
}`

// legacyPaymentsSpec is Swagger 2.0 on purpose: most estates still have a few
// services on an old framework, and the portal must index them too.
const legacyPaymentsSpec = `{
  "swagger": "2.0",
  "info": {
    "title": "Legacy Payments",
    "description": "Старый платёжный шлюз. Новые интеграции — через Billing API.",
    "version": "1.0.7"
  },
  "host": "localhost:7105",
  "basePath": "/",
  "schemes": ["http"],
  "consumes": ["application/json"],
  "produces": ["application/json"],
  "paths": {
    "/payments": {
      "get": {
        "operationId": "listPayments",
        "summary": "Список платежей",
        "parameters": [
          {"name": "from", "in": "query", "required": false, "type": "string", "format": "date"},
          {"name": "to", "in": "query", "required": false, "type": "string", "format": "date"}
        ],
        "responses": {
          "200": {"description": "OK", "schema": {"type": "array", "items": {"$ref": "#/definitions/Payment"}}}
        }
      },
      "post": {
        "operationId": "createPayment",
        "summary": "Провести платёж",
        "parameters": [
          {"name": "body", "in": "body", "required": true, "schema": {"$ref": "#/definitions/PaymentRequest"}}
        ],
        "responses": {
          "201": {"description": "Created", "schema": {"$ref": "#/definitions/Payment"}},
          "400": {"description": "Bad request", "schema": {"$ref": "#/definitions/Error"}}
        }
      }
    },
    "/payments/{id}": {
      "parameters": [{"name": "id", "in": "path", "required": true, "type": "string"}],
      "get": {
        "operationId": "getPayment",
        "summary": "Платёж по идентификатору",
        "responses": {
          "200": {"description": "OK", "schema": {"$ref": "#/definitions/Payment"}},
          "404": {"description": "Not found", "schema": {"$ref": "#/definitions/Error"}}
        }
      }
    }
  },
  "definitions": {
    "Payment": {
      "type": "object",
      "required": ["id", "amount", "state"],
      "properties": {
        "id": {"type": "string"},
        "amount": {"type": "number", "format": "double"},
        "currency": {"type": "string"},
        "state": {"type": "string", "enum": ["pending", "ok", "failed"]}
      }
    },
    "PaymentRequest": {
      "type": "object",
      "required": ["amount", "cardToken"],
      "properties": {
        "amount": {"type": "number", "format": "double"},
        "currency": {"type": "string", "default": "RUB"},
        "cardToken": {"type": "string"}
      }
    },
    "Error": {
      "type": "object",
      "properties": {"code": {"type": "string"}, "message": {"type": "string"}}
    }
  }
}`

const analyticsSpec = `{
  "openapi": "3.0.3",
  "info": {
    "title": "Analytics API",
    "description": "Отчёты и выгрузки. Спека отдаётся в YAML.",
    "version": "5.1.0"
  },
  "servers": [{"url": "http://localhost:7106"}],
  "paths": {
    "/reports": {
      "get": {
        "operationId": "listReports",
        "summary": "Доступные отчёты",
        "responses": {
          "200": {"description": "OK", "content": {"application/json": {"schema": {
            "type": "array", "items": {"$ref": "#/components/schemas/Report"}}}}}
        }
      }
    },
    "/reports/{id}": {
      "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
      "get": {
        "operationId": "getReport",
        "summary": "Описание отчёта",
        "responses": {
          "200": {"description": "OK", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Report"}}}},
          "404": {"description": "Not found"}
        }
      }
    },
    "/reports/{id}/run": {
      "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
      "post": {
        "operationId": "runReport",
        "summary": "Запустить отчёт",
        "requestBody": {"required": false, "content": {"application/json": {"schema": {
          "type": "object",
          "properties": {"from": {"type": "string", "format": "date"}, "to": {"type": "string", "format": "date"}}}}}},
        "responses": {
          "202": {"description": "Accepted", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/RunResult"}}}},
          "404": {"description": "Not found"}
        }
      }
    }
  },
  "components": {
    "schemas": {
      "Report": {
        "type": "object",
        "required": ["id", "title"],
        "properties": {
          "id": {"type": "string"},
          "title": {"type": "string"},
          "updatedAt": {"type": "string", "format": "date-time"}
        }
      },
      "RunResult": {
        "type": "object",
        "required": ["runId", "state"],
        "properties": {
          "runId": {"type": "string"},
          "state": {"type": "string", "enum": ["queued", "running", "done", "failed"]}
        }
      }
    }
  }
}`

const warehouseSpec = `{
  "openapi": "3.0.3",
  "info": {
    "title": "Warehouse API",
    "description": "Приёмка и отгрузка. Спека лежит на нестандартном пути — портал находит её через configUrl со страницы /swagger.",
    "version": "1.1.0"
  },
  "servers": [{"url": "http://localhost:7107"}],
  "paths": {
    "/shipments": {
      "get": {
        "operationId": "listShipments",
        "summary": "Отгрузки",
        "parameters": [{"name": "state", "in": "query", "required": false, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "OK", "content": {"application/json": {"schema": {
            "type": "array", "items": {"$ref": "#/components/schemas/Shipment"}}}}}
        }
      },
      "post": {
        "operationId": "createShipment",
        "summary": "Создать отгрузку",
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Shipment"}}}},
        "responses": {"201": {"description": "Created"}, "400": {"description": "Bad request"}}
      }
    }
  },
  "components": {
    "schemas": {
      "Shipment": {
        "type": "object",
        "required": ["id", "state"],
        "properties": {
          "id": {"type": "string"},
          "state": {"type": "string", "enum": ["packing", "ready", "shipped"]},
          "carrier": {"type": "string"}
        }
      }
    }
  }
}`
