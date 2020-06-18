# Test Plan File Format

Each test plan yaml file has multiple test suites separated by '---'. Each test suite can have multiple tests. In the following example, the name of the test suite is "/store/order". The test suites are executed in sequential order.

```yml
---
/store/order:
- name: post_placeOrder_1
  path: /store/order
  method: post
- name: get_getOrderById_2
  path: /store/order/{orderId}
  method: get
- name: delete_deleteOrder_3
  path: /store/order/{orderId}
  method: delete
- name: get_getOrderById_4
  path: /store/order/{orderId}
  method: get
  expect:
    status: fail
  pathParams:
    orderId: '{{delete_deleteOrder_3.pathParams.orderId}}'
```

In this test suite, there are four tests, triggering the following REST calls to the host specified in the OpenAPI spec. 

* POST /store/order
* GET /store/order/{orderId}
* DELETE /store/order/{orderId}
* GET /store/order/{orderId}

The last test tries to get the order we just deleted, and expects to get a failure. In this case it explicitly sets a path parameter. The following keywords are allowed, mapping to the respective REST call parameter location.

* pathParams
* queryParams
* bodyParams
* formParams
* headerParams

When setting parameters, the value can be either a explicit value, or a template. A template has the format of '{{testName.parameterLocation.parameterName...}}'.

* testName - the name of a test.
* parameterLocation - where the parameter comes from. It can be either one of pathParams, queryParams, bodyParams, formParams, headerParams, outputs.
* parameterName - the name to look for under parameterLocation whose value is to be used as this template's value. This name can be in the form of "object.property.property...". When parameterName is just one single value without any ".", meqa will try to find a named entity that matches the parameterName.

In the above example, the template '{{delete_deleteOrder_3.pathParams.orderId}}' maps to the "orderId" path param of test "delete_deleteOrder_3".

As another example, the last test can use the following parameter template to achieve the same result:

```yml
- name: get_getOrderById_4
  path: /store/order/{orderId}
  method: get
  expect:
    status: fail
  pathParams:
    orderId: '{{post_placeOrder_1.outputs.id}}'
```

## Test Plan Init Section

The first test suite can have a special "meqa_init" name. The parameters under meqa_init will be applied to all the test suites in the same file. For instance, in the following code that runs against bitbucket's API, we tell all the tests to use a specific username and repo_slug.

```yml
---
meqa_init:
- name: meqa_init
  pathParams:
    username: meqatest
    repo_slug: swagger_repo_1
```

Similarly, each test suite can have its own meqa_init section, to set a parameter for all the tests in that test suite. For instance, the following will hardcode all the "orderId" values in path to be 800800, as well as all the "id" values in body.

```yml
/store/order:
- name: meqa_init
  pathParams:
    orderId: 800800
  bodyParams:
    id: 800800
- name: post_placeOrder_1
  path: /store/order
  method: post
- name: get_getOrderById_2
  path: /store/order/{orderId}
  method: get
```

## Test Result File

When running mqgo you must provide a meqa directory through "-d" option. In this directory you will find a result.yml file after you do "mqgo run". The result.yml has the same format as the test plan file, and lists all the tests in the last run, with all the parameter and expect values being the actual vaules used.

Besides checking the actual values returned from the REST server, you can also feed result.yml back to "mqgo run" as the input test plan file through "-p". This allows you to check whether the same input will always get the same output.
